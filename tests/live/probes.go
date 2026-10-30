//go:build with_gvisor && tailrelay_live

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mdns "github.com/miekg/dns"
)

func prepareAcceptanceState(root, directory string) (string, error) {
	root, directory = filepath.Clean(root), filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || !filepath.IsAbs(directory) || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("acceptance state must be a child of the acceptance directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", errors.New("cannot prepare acceptance directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("acceptance directory must be a real directory")
	}
	// Root operations prevent symlink and traversal races from opening a
	// directory outside this boundary, including an already existing link.
	boundary, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("cannot open acceptance directory")
	}
	defer boundary.Close()
	if err := boundary.MkdirAll(relative, 0o700); err != nil {
		return "", errors.New("cannot prepare private acceptance state")
	}
	state, err := boundary.OpenFile(relative, os.O_RDONLY, 0)
	if err != nil {
		return "", errors.New("cannot open contained acceptance state")
	}
	defer state.Close()
	if info, err := state.Stat(); err != nil || !info.IsDir() {
		return "", errors.New("acceptance state must be a directory")
	}
	if err := state.Chmod(0o700); err != nil {
		return "", errors.New("cannot protect acceptance state")
	}
	return filepath.Join(root, relative), nil
}

func newDNSProbe() (*mdns.Msg, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("cannot generate an uncached DNS probe")
	}
	query := new(mdns.Msg)
	query.SetQuestion(fmt.Sprintf("%x.tailrelay-probe.example.com.", nonce), mdns.TypeA)
	return query, nil
}

func validDNSProbeReply(query, reply *mdns.Msg) bool {
	if !reply.Response || reply.Id != query.Id || len(reply.Question) != 1 || len(query.Question) != 1 {
		return false
	}
	question, expected := reply.Question[0], query.Question[0]
	if !strings.EqualFold(question.Name, expected.Name) ||
		question.Qtype != expected.Qtype || question.Qclass != expected.Qclass {
		return false
	}
	// A fresh random name normally returns NXDOMAIN. Its matching DNS reply
	// proves the query completed; a cached answer from a previous run cannot.
	return reply.Rcode == mdns.RcodeSuccess || reply.Rcode == mdns.RcodeNameError
}
