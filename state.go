package tailrelay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (c Credentials) validate() error {
	for _, value := range []string{c.Username, c.Password} {
		if len(value) < 1 || len(value) > 255 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("proxy username and password must each contain 1..255 bytes without control characters")
		}
	}
	return nil
}

func LoadCredentials(path string) (Credentials, error) {
	wire, err := readCredentialFile(path, 4096)
	if err != nil {
		return Credentials{}, errors.New("cannot read proxy credentials: require a private file or read-only systemd credential")
	}
	var credentials Credentials
	decoder := json.NewDecoder(bytes.NewReader(wire))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return credentials, errors.New("proxy credentials must be a JSON object with username and password")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || (key != "username" && key != "password") {
			return Credentials{}, errors.New("proxy credentials contain an unknown or duplicate field")
		}
		seen[key] = true
		value := &credentials.Username
		if key == "password" {
			value = &credentials.Password
		}
		if err := decoder.Decode(value); err != nil {
			return Credentials{}, errors.New("proxy credential values must be strings")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return Credentials{}, errors.New("invalid proxy credentials JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Credentials{}, errors.New("proxy credentials contain trailing data")
	}
	return credentials, credentials.validate()
}

func loadAuthKey(config TailscaleConfig) (string, error) {
	key := ""
	if config.AuthKeyFile != "" {
		wire, err := readCredentialFile(config.AuthKeyFile, 4096)
		if err != nil {
			return "", errors.New("cannot read private Tailscale auth key file")
		}
		key = strings.TrimSpace(string(wire))
		if key == "" {
			return "", errors.New("Tailscale auth key file is empty")
		}
	} else if config.AuthKeyEnv != "" {
		// An unset bootstrap env is fine after the device identity is saved.
		key = os.Getenv(config.AuthKeyEnv)
	}
	if key != "" && (!strings.HasPrefix(key, "tskey-auth-") || strings.ContainsAny(key, " \t\r\n\x00") || len(key) > 4096) {
		return "", errors.New("expected a Tailscale device auth key, not an API access token")
	}
	return key, nil
}

// O_NOFOLLOW and fstat validate the opened inode, not a path checked before
// opening. State and ordinary private files must belong to this process user.
func openPrivate(path string, flags int, mode uint32) (*os.File, error) {
	return openSecret(path, flags, mode, false)
}

func openSecret(path string, flags int, mode uint32, allowSystemd bool) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	var stat unix.Stat_t
	if err != nil || unix.Fstat(fd, &stat) != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("secret must be a regular file")
	}
	private := info.Mode().Perm()&0o077 == 0 && stat.Uid == uint32(os.Geteuid())
	if !private && !(allowSystemd && isSystemdCredential(file, path, info)) {
		file.Close()
		return nil, errors.New("file must be private to this user or a read-only systemd credential")
	}
	return file, nil
}

func readPrivate(path string, limit int64) ([]byte, error) {
	file, err := openPrivate(path, unix.O_RDONLY, 0)
	return readSecret(file, err, limit)
}

func readCredentialFile(path string, limit int64) ([]byte, error) {
	file, err := openSecret(path, unix.O_RDONLY, 0, true)
	return readSecret(file, err, limit)
}

func readSecret(file *os.File, err error, limit int64) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	defer file.Close()
	wire, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(wire)) > limit {
		return nil, errors.New("cannot read private file or file is too large")
	}
	return wire, nil
}

type stateLease struct {
	file *os.File
	once sync.Once
}

func lockState(config Config) (*stateLease, error) {
	if err := privateDirectory(config.StateDir); err != nil {
		return nil, err
	}
	file, err := openPrivate(filepath.Join(config.StateDir, "runner.lock"), unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return nil, errors.New("cannot open private state lock")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("state_dir is already in use; every instance needs its own directory")
	}
	lease := &stateLease{file: file}
	ok := false
	defer func() {
		if !ok {
			lease.Close()
		}
	}()
	identityPath := filepath.Join(config.StateDir, "instance.json")
	wire, err := readPrivate(identityPath, 4096)
	if errors.Is(err, os.ErrNotExist) {
		wire, _ = json.Marshal(struct {
			Name string `json:"name"`
		}{Name: config.Name})
		err = atomicWrite(identityPath, wire)
	} else if err == nil {
		var identity struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(wire, &identity) != nil || identity.Name != config.Name {
			err = errors.New("state_dir belongs to another instance; use a separate directory")
		}
	}
	if err != nil {
		return nil, err
	}
	for _, child := range []string{"tailscale", "taildrop"} {
		if err := privateDirectory(filepath.Join(config.StateDir, child)); err != nil {
			return nil, err
		}
	}
	ok = true
	return lease, nil
}

func privateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("cannot create private state directory")
	}
	info, err := os.Lstat(directory)
	var stat unix.Stat_t
	if err != nil || unix.Lstat(directory, &stat) != nil || !info.IsDir() ||
		info.Mode().Perm() != 0o700 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("state directories must be real directories, owner-only (0700) and owned by this user")
	}
	return nil
}

func (s *stateLease) Close() {
	s.once.Do(func() {
		_ = unix.Flock(int(s.file.Fd()), unix.LOCK_UN)
		_ = s.file.Close()
	})
}

func atomicWrite(path string, wire []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tailrelay-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(wire); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
