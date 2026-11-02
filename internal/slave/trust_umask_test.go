//go:build unix

package slave

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

// The systemd unit runs the slave with UMask=0077; sshd reads the trust files as another user
// (root squashed to nobody on NFS), so every published file must still be world-readable.
func TestPublishTrustIgnoresPrivateUmask(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	pub, _, _ := sshKey(t)
	s := replication.Snapshot{Version: replication.Version, CAs: []replication.CA{testCA(t), {ID: 2, Name: "ssh", Kind: "ssh", Enabled: true, CertificatePEM: pub}},
		SSHRevocations: []replication.Revocation{{CAID: 2, SerialHex: "ff"}}}
	if err := PublishTrust(dir, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, err := filepath.EvalSymlinks(filepath.Join(dir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(current, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o644)
		if d.IsDir() {
			want = 0o755
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
