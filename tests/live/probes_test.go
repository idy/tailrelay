//go:build with_gvisor && tailrelay_live

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mdns "github.com/miekg/dns"
)

func TestAcceptanceStateCannotChangeOutsidePermissions(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "acceptance"), filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		root, filepath.Join(root, ".."), root + "/../../..",
		filepath.Join(root, "..", "outside"), "relative/instance",
	} {
		if _, err := prepareAcceptanceState(root, directory); err == nil {
			t.Fatalf("unsafe state accepted: %s", directory)
		}
	}
	if _, err := prepareAcceptanceState(root, filepath.Join(root, "instance")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAcceptanceState(root, link); err == nil {
		t.Fatal("state symlink escaped the acceptance directory")
	}
	if _, err := prepareAcceptanceState(root, filepath.Join(link, "created")); err == nil {
		t.Fatal("state parent symlink escaped the acceptance directory")
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatal("validation changed outside directory permissions")
	}
	if _, err := os.Stat(filepath.Join(outside, "created")); !os.IsNotExist(err) {
		t.Fatal("validation created state outside the acceptance directory")
	}
}

func TestAcceptanceStateNormalizesAndProtectsContainedDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "acceptance")
	directory := root + "/intermediate/../instance"
	got, err := prepareAcceptanceState(root, directory)
	if err != nil || got != filepath.Join(root, "instance") {
		t.Fatalf("contained path: got=%s err=%v", got, err)
	}
	if info, err := os.Stat(got); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("acceptance state is not private")
	}
}

func TestDNSProbeDoesNotReuseTheHealthyRunCacheKey(t *testing.T) {
	first, err := newDNSProbe()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newDNSProbe()
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(first.Question[0].Name, second.Question[0].Name) {
		t.Fatal("outage query can hit the healthy run's DNS cache")
	}
	reply := new(mdns.Msg)
	reply.SetRcode(first, mdns.RcodeNameError)
	if !validDNSProbeReply(first, reply) || validDNSProbeReply(second, reply) {
		t.Fatal("DNS probe accepted a cached reply for another question")
	}
	reply.SetRcode(first, mdns.RcodeServerFailure)
	if validDNSProbeReply(first, reply) {
		t.Fatal("DNS probe accepted an upstream failure")
	}
	reply.SetReply(first)
	reply.Id++
	if validDNSProbeReply(first, reply) {
		t.Fatal("DNS probe accepted a mismatched response ID")
	}
}
