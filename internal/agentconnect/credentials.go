package agentconnect

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

func secureEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\t") {
		return "", errors.New("provide a base URL without credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("HTTPS is required except for explicit loopback HTTP URLs")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func credentialName(endpoint, profile string) (string, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(profile) {
		return "", errors.New("profile must contain 1–64 letters, digits, underscores or hyphens")
	}
	sum := sha256.Sum256([]byte(endpoint + "\n" + profile))
	return hex.EncodeToString(sum[:]) + ".enc", nil
}

// Credentials are AES-GCM encrypted. The independent per-user key is kept out
// of projects, in a 0600 file under a private 0700 directory. This protects
// copied credential data, not a compromised account that can read both files.
func credentialRoot(create bool) (*os.Root, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	const dir = ".release-control"
	info, err := root.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		err = root.Mkdir(dir, 0700)
		if err == nil {
			info, err = root.Lstat(dir)
		}
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("credential directory must be a private directory (0700), not a symlink")
	}
	return root.OpenRoot(dir)
}

func readPrivate(root *os.Root, name string) ([]byte, error) {
	if err := safePath(root, name); err != nil {
		return nil, err
	}
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, errors.New("credential file must be a private regular file (0600)")
	}
	return root.ReadFile(name)
}

func credentialKey(root *os.Root, create bool) ([]byte, error) {
	key, err := readPrivate(root, "key")
	if errors.Is(err, os.ErrNotExist) && create {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := root.OpenFile("key", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(e, os.ErrExist) {
			return credentialKey(root, false)
		}
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid credential encryption key")
	}
	return key, nil
}

func credentialCipher(root *os.Root, create bool) (cipher.AEAD, error) {
	key, err := credentialKey(root, create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func saveCredential(endpoint, profile, token string) error {
	name, err := credentialName(endpoint, profile)
	if err != nil {
		return err
	}
	root, err := credentialRoot(true)
	if err != nil {
		return err
	}
	defer root.Close()
	aead, err := credentialCipher(root, true)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	data := aead.Seal(nonce, nonce, []byte(token), []byte(endpoint+"\n"+profile))
	if err = safePath(root, name); err != nil {
		return err
	}
	suffix := make([]byte, 16)
	if _, err = rand.Read(suffix); err != nil {
		return err
	}
	temp := ".tmp-" + hex.EncodeToString(suffix)
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(temp, name)
}

func loadCredential(endpoint, profile string) (string, error) {
	name, err := credentialName(endpoint, profile)
	if err != nil {
		return "", err
	}
	root, err := credentialRoot(false)
	if err != nil {
		return "", err
	}
	defer root.Close()
	// Read the profile first so a missing profile never creates a key.
	data, err := readPrivate(root, name)
	if err != nil {
		return "", err
	}
	aead, err := credentialCipher(root, false)
	if err != nil {
		return "", err
	}
	if len(data) < aead.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(endpoint+"\n"+profile))
	if err != nil {
		return "", errors.New("credential decryption failed")
	}
	if len(plain) == 0 {
		return "", errors.New("empty credential")
	}
	return string(plain), nil
}

// ResolveCredential loads an explicitly named environment variable or a saved
// device credential, never a project-local plaintext token.
func ResolveCredential(raw, profile, tokenEnv string) (string, error) {
	endpoint, err := secureEndpoint(raw)
	if err != nil {
		return "", err
	}
	if tokenEnv != "" {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(tokenEnv) {
			return "", errors.New("invalid token environment variable name")
		}
		token := os.Getenv(tokenEnv)
		if token == "" {
			return "", errors.New("token environment variable is empty")
		}
		return token, nil
	}
	if profile == "" {
		profile = "default"
	}
	token, err := loadCredential(endpoint, profile)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no saved agent credential; run release-control login --url %s --profile %s", endpoint, profile)
	}
	return token, err
}
