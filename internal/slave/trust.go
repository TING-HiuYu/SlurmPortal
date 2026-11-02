package slave

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

// PublishTrust writes one immutable generation of public trust material and atomically repoints
// "current". Layout matches v1: current/ca-bundle.pem, current/crl/ca-<id>.pem,
// current/ssh/trusted-user-ca-keys.pub and current/ssh/revoked.krl.
func PublishTrust(shareDir string, s replication.Snapshot, now time.Time) error {
	if shareDir == "" {
		return errors.New("trust share directory is required")
	}
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(shareDir, 0o755); err != nil {
		return err
	}
	var (
		active    []replication.CA
		bundle    []byte
		sshBundle []byte
		sshCAs    []replication.CA
	)
	crls := map[string][]byte{}
	specs := map[int64][]byte{}
	for _, ca := range s.CAs {
		if !ca.Enabled {
			continue
		}
		if ca.Kind == "ssh" {
			pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(ca.CertificatePEM))
			if err != nil || strings.TrimSpace(string(rest)) != "" || pub.Type() != ssh.KeyAlgoED25519 {
				return fmt.Errorf("SSH CA %d has an invalid Ed25519 public key", ca.ID)
			}
			active, sshCAs = append(active, ca), append(sshCAs, ca)
			sshBundle = append(sshBundle, ssh.MarshalAuthorizedKey(pub)...)
			continue
		}
		cb, rest := pem.Decode([]byte(ca.CertificatePEM))
		if cb == nil || cb.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
			return fmt.Errorf("CA %d has invalid certificate PEM", ca.ID)
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
			return fmt.Errorf("CA %d cannot sign CRLs", ca.ID)
		}
		lb, rest := pem.Decode([]byte(ca.CRLPEM))
		if lb == nil || lb.Type != "X509 CRL" || strings.TrimSpace(string(rest)) != "" {
			return fmt.Errorf("CA %d has invalid CRL PEM", ca.ID)
		}
		crl, err := x509.ParseRevocationList(lb.Bytes)
		if err != nil {
			return fmt.Errorf("parse CRL for CA %d: %w", ca.ID, err)
		}
		if err := crl.CheckSignatureFrom(cert); err != nil {
			return fmt.Errorf("verify CRL for CA %d: %w", ca.ID, err)
		}
		if crl.NextUpdate.Before(now) || crl.Number == nil || !crl.Number.IsInt64() || crl.Number.Int64() != ca.CRLNumber {
			return fmt.Errorf("CA %d CRL is expired or has an unexpected number", ca.ID)
		}
		active = append(active, ca)
		bundle = append(bundle, []byte(ca.CertificatePEM)...)
		if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
			bundle = append(bundle, '\n')
		}
		crls["ca-"+strconv.FormatInt(ca.ID, 10)+".pem"] = []byte(ca.CRLPEM)
	}
	sshIDs := map[int64]bool{}
	for _, ca := range sshCAs {
		sshIDs[ca.ID] = true
	}
	var revs []replication.Revocation
	for _, r := range s.SSHRevocations {
		if !sshIDs[r.CAID] {
			continue
		}
		serial, ok := new(big.Int).SetString(r.SerialHex, 16)
		if !ok || serial.Sign() <= 0 || serial.BitLen() > 64 {
			return fmt.Errorf("SSH CA %d has an invalid revoked serial", r.CAID)
		}
		specs[r.CAID] = append(specs[r.CAID], []byte("serial: "+serial.String()+"\n")...)
		revs = append(revs, r)
	}
	content, err := json.Marshal(struct {
		CAs  []replication.CA         `json:"cas"`
		Revs []replication.Revocation `json:"sshRevocations"`
	}{active, revs})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(content)
	version := hex.EncodeToString(sum[:])
	target := filepath.Join(shareDir, version)
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err := stageGeneration(shareDir, target, bundle, crls, sshCAs, sshBundle, specs); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// A generation staged by an older slave may carry private modes; repair it before publishing.
	if err := publicModes(target); err != nil {
		return err
	}
	link := filepath.Join(shareDir, ".current-"+version)
	if err := os.Symlink(version, link); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Rename(link, filepath.Join(shareDir, "current"))
}

func stageGeneration(shareDir, target string, bundle []byte, crls map[string][]byte, sshCAs []replication.CA, sshBundle []byte, specs map[int64][]byte) error {
	stage, err := os.MkdirTemp(shareDir, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(stage, "crl"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "ca-bundle.pem"), bundle, 0o644); err != nil {
		return err
	}
	for name, data := range crls {
		if err := os.WriteFile(filepath.Join(stage, "crl", name), data, 0o644); err != nil {
			return err
		}
	}
	if len(sshCAs) > 0 {
		dir := filepath.Join(stage, "ssh")
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "trusted-user-ca-keys.pub"), sshBundle, 0o644); err != nil {
			return err
		}
		krl := filepath.Join(dir, "revoked.krl")
		for i, ca := range sshCAs {
			name := "ca-" + strconv.FormatInt(ca.ID, 10)
			pubPath := filepath.Join(dir, name+".pub")
			if err := os.WriteFile(pubPath, []byte(ca.CertificatePEM), 0o644); err != nil {
				return err
			}
			specPath := filepath.Join(dir, name+".spec")
			if err := os.WriteFile(specPath, specs[ca.ID], 0o644); err != nil {
				return err
			}
			args := []string{"-k", "-f", krl}
			if i > 0 {
				args = append(args, "-u")
			}
			args = append(args, "-s", pubPath, specPath)
			if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("generate OpenSSH KRL: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	if err := publicModes(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, target); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// publicModes makes a generation world-readable whatever the umask: the slave runs with a private
// umask, and sshd (often root squashed on NFS), gateways and CRL consumers read these files.
func publicModes(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Chmod(path, 0o755)
		case d.Type().IsRegular():
			return os.Chmod(path, 0o644)
		}
		return nil
	})
}
