package transport_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

// A separate opt-in smoke permits inspecting an image already admitted by the
// full delivery fixture. It makes no new admission or pipeline assertion.
func TestRealApplicationAdmittedRuntime(t *testing.T) {
	image := os.Getenv("RCP_TEST_REALAPP_ADMITTED_IMAGE")
	config := os.Getenv("RCP_TEST_REALAPP_CONFIG")
	if os.Getenv("RCP_TEST_DOCKER") != "1" || image == "" || config == "" {
		t.Skip("explicit previously admitted cached image and profile required")
	}
	if !regexp.MustCompile(`^127\.0\.0\.1:[0-9]+/fixture/rcp@sha256:[0-9a-f]{64}$`).MatchString(image) {
		t.Fatal("exact local fixture image digest required")
	}
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", append([]string{"--context", "orbstack"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("owned runtime fixture: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	parts := strings.Split(image, "@")
	origin := "http://" + strings.TrimSuffix(parts[0], "/fixture/rcp")
	runRealApplicationRuntime(t, docker, t.TempDir(), fmt.Sprintf("realapp-smoke-%d", time.Now().UnixNano()), origin, domain.DeliveryArtifact{Digest: parts[1]}, config)
}

// Runtime credentials and TLS keys exist only in this private fixture. Neither
// the selected public runtime tree nor the prepared toolchain contains them.
func runRealApplicationRuntime(t *testing.T, docker func(...string) string, root, owner, origin string, artifact domain.DeliveryArtifact, configPath string) {
	t.Helper()
	image := strings.TrimPrefix(origin, "http://") + "/fixture/rcp@" + artifact.Digest
	// Prefer the exact cached immutable image; a local fixture registry may have
	// been cleaned after admission. A missing image may only use that local pin.
	inspect := exec.Command("docker", "--context", "orbstack", "image", "inspect", image)
	if err := inspect.Run(); err != nil {
		docker("pull", image)
	}
	network := docker("network", "create", "--internal", "--label", "rcp.fixture.owner="+owner, owner)
	t.Cleanup(func() {
		if docker("network", "inspect", "--format", `{{.Id}} {{index .Labels "rcp.fixture.owner"}}`, network) != network+" "+owner {
			t.Error("network ownership changed")
			return
		}
		docker("network", "rm", network)
	})
	owned := func(args ...string) string {
		t.Helper()
		// OrbStack otherwise edits CA files even in readonly containers at start.
		// This documented per-container opt-out preserves the admitted bytes.
		id := docker(append([]string{"run", "-d", "--label", "rcp.fixture.owner=" + owner, "--label", "dev.orbstack.add-ca-certificates=false", "--network", network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "512m", "--cpus", "1", "--pids-limit", "128"}, args...)...)
		t.Logf("owned runtime fixture id=%s", id)
		t.Cleanup(func() {
			if docker("inspect", "--format", `{{.Id}} {{index .Config.Labels "rcp.fixture.owner"}}`, id) != id+" "+owner {
				t.Error("runtime ownership changed")
				return
			}
			docker("rm", "--force", id)
		})
		return id
	}
	tlsDir := filepath.Join(root, "private-tls")
	if e := os.Mkdir(tlsDir, 0700); e != nil {
		t.Fatal(e)
	}
	makeCA := func(name string) (*x509.Certificate, *rsa.PrivateKey, []byte) {
		t.Helper()
		key, e := rsa.GenerateKey(rand.Reader, 2048)
		if e != nil {
			t.Fatal(e)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
		der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		certificate, _ := x509.ParseCertificate(der)
		return certificate, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	ca, caKey, caPEM := makeCA("private fixture CA")
	serverKey, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "tls-fixture"}, DNSNames: []string{"tls-fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, template, ca, &serverKey.PublicKey, caKey)
	if e != nil {
		t.Fatal(e)
	}
	write := func(name string, b []byte) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(tlsDir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("tls.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write("tls.key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}))
	write("server.py", []byte(`import http.server,json,ssl
class Handler(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def do_POST(self):
  self.rfile.read(int(self.headers.get('Content-Length','0')))
  if self.path!='/app/installations/2/access_tokens': self.send_error(404);return
  self.send_response(201);self.end_headers();self.wfile.write(json.dumps({'token':'private-fixture-token','expires_at':'2030-01-01T00:00:00Z'}).encode())
 def do_GET(self):
  if not self.path.startswith('/installation/repositories?'): self.send_error(404);return
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps({'repositories':[{'id':11,'full_name':'fixture/runtime','default_branch':'main','private':True}]}).encode())
context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);context.load_cert_chain('/fixture/tls.crt','/fixture/tls.key')
class Server(http.server.HTTPServer):
 def get_request(self):
  sock,address=self.socket.accept()
  try: return context.wrap_socket(sock,server_side=True),address
  except ssl.SSLError as error:
   print(json.dumps({'tls_alert':error.reason}),flush=True);sock.close();raise
server=Server(('0.0.0.0',8443),Handler);server.serve_forever()
`))
	clientID := owned("--network-alias", "tls-fixture", "--mount", "type=bind,source="+tlsDir+",target=/fixture,readonly", "--entrypoint", "/usr/bin/python3", "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", "/fixture/server.py")
	publicCA, e := os.ReadFile(filepath.Join(filepath.Dir(configPath), "runtime-tree/etc/ssl/certs/ca-certificates.crt"))
	if e != nil {
		t.Fatal(e)
	}
	write("augmented-ca.pem", append(append([]byte{}, publicCA...), caPEM...))
	_, _, wrongCA := makeCA("wrong private fixture CA")
	write("wrong-ca.pem", append(append([]byte{}, publicCA...), wrongCA...))
	// These two files contain certificates only. The nonroot application may
	// read the private fixture mount; private TLS/App keys retain mode 0600.
	for _, name := range []string{"augmented-ca.pem", "wrong-ca.pem"} {
		if err := os.Chmod(filepath.Join(tlsDir, name), 0444); err != nil {
			t.Fatal(err)
		}
	}
	appKey, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	// Only the test child process receives this fake key. It is never written into
	// an image, an RCP command, a normal trust bundle, or a fixture log.
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)}))
	token := "private-runtime-fixture-token-0000000001"
	request := func(base, method, path string, body []byte, auth bool) (int, []byte) {
		t.Helper()
		headers := map[string]string{"Content-Type": "application/json"}
		if auth {
			headers["Authorization"] = "Bearer " + token
		}
		input, _ := json.Marshal(map[string]any{"url": base + path, "method": method, "headers": headers, "body": string(body)})
		out := docker("exec", clientID, "/usr/bin/python3", "-c", `import json,sys,urllib.request,urllib.error
v=json.loads(sys.argv[1]);r=urllib.request.Request(v['url'],data=v['body'].encode() if v['method']!='GET' else None,headers=v['headers'],method=v['method'])
try: response=urllib.request.urlopen(r,timeout=10)
except urllib.error.HTTPError as error: response=error
print(json.dumps({'status':response.status,'body':response.read(2097152).decode()}))`, string(input))
		var result struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		return result.Status, []byte(result.Body)
	}
	post := func(base string, c domain.Command) {
		t.Helper()
		c.Actor = "fixture/operator"
		body, _ := json.Marshal(c)
		status, data := request(base, "POST", "/api/commands", body, true)
		if status != 200 {
			t.Fatalf("runtime %s returned%d:%s", c.Action, status, data)
		}
	}
	for _, mode := range []string{"default", "wrong", "augmented"} {
		args := []string{"--network-alias", "runtime-" + mode, "--user", "65532:65532", "--tmpfs", "/data:rw,size=67108864,uid=65532,gid=65532", "--tmpfs", "/workspace:rw,size=67108864,uid=65532,gid=65532", "--tmpfs", "/tmp:rw,size=33554432,uid=65532,gid=65532", "-e", "RC_TOKEN=" + token, "-e", "RCP_FIXTURE_APP_KEY=" + keyPEM, "-e", "RCP_GITHUB_API_URL=https://tls-fixture:8443", "-e", "RCP_DATA_PATH=/data/state.db", "-e", "RCP_WORKSPACE_ROOT=/workspace", "-e", "LISTEN_ADDR=:8080"}
		args = append(args, "-e", "ALLOWED_HOSTS=runtime-"+mode)
		if mode != "default" {
			file := "wrong-ca.pem"
			if mode == "augmented" {
				file = "augmented-ca.pem"
			}
			args = append(args, "--mount", "type=bind,source="+filepath.Join(tlsDir, file)+",target=/fixture-ca.pem,readonly", "-e", "SSL_CERT_FILE=/fixture-ca.pem")
		}
		id := owned(append(args, image)...)
		base := "http://runtime-" + mode + ":8080"
		docker("exec", clientID, "/usr/bin/python3", "-c", `import sys,time,urllib.request
for attempt in range(100):
 try:
  response=urllib.request.urlopen(sys.argv[1]+'/healthz',timeout=1)
  if response.status==200: sys.exit(0)
 except Exception: pass
 time.sleep(.1)
sys.exit('admitted actual RCP image never became healthy')`, base)
		// A normal image retains the byte-exact public roots; fake trust is mounted
		// separately only for this process. Go's provider uses SystemCertPool.
		actualCA := docker("exec", id, "/usr/bin/git", "hash-object", "/etc/ssl/certs/ca-certificates.crt")
		expectedHash := fmt.Sprintf("%x", sha256.Sum256(publicCA))
		blob := append([]byte(fmt.Sprintf("blob %d\x00", len(publicCA))), publicCA...)
		if actualCA != fmt.Sprintf("%x", sha1.Sum(blob)) {
			t.Fatalf("public CA identity mismatch: git=%q expected=%x public_bytes=%d", actualCA, sha1.Sum(blob), len(publicCA))
		}
		status, _ := request(base, "GET", "/api/state", nil, false)
		if status != 401 {
			t.Fatalf("unprotected runtime API:%d", status)
		}
		post(base, domain.Command{Action: "create_product", ID: "smoke-p", Data: map[string]any{"name": "private runtime fixture"}})
		post(base, domain.Command{Action: "create_feature", ID: "smoke-f", ProductID: "smoke-p", Data: map[string]any{"title": "smoke", "goal": "actual image"}})
		post(base, domain.Command{Action: "update_feature", FeatureID: "smoke-f", Data: map[string]any{"title": "verified actual image"}})
		status, data := request(base, "GET", "/api/products/smoke-p/context", nil, true)
		if status != 200 || !bytes.Contains(data, []byte("private runtime fixture")) {
			t.Fatalf("runtime Product readback:%d %s", status, data)
		}
		status, data = request(base, "GET", "/api/features/smoke-f/context", nil, true)
		if status != 200 || !bytes.Contains(data, []byte("verified actual image")) {
			t.Fatalf("runtime Feature update readback:%d %s", status, data)
		}
		status, data = request(base, "GET", "/app.js", nil, false)
		if status != 200 || !bytes.Contains(data, []byte("accepted393 changed-source fixture")) {
			t.Fatalf("actual changed embedded asset missing:%d", status)
		}
		docker("exec", id, "/usr/bin/git", "init", "/workspace/sample")
		docker("exec", id, "/usr/bin/git", "-C", "/workspace/sample", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "private fixture")
		docker("exec", id, "/usr/bin/git", "-C", "/workspace/sample", "remote", "add", "origin", "https://github.com/fixture/runtime")
		body, _ := json.Marshal(map[string]string{"product_id": "smoke-p", "actor": "fixture"})
		status, data = request(base, "POST", "/api/workspaces/scan", body, true)
		if status != 200 || !bytes.Contains(data, []byte("sample")) {
			t.Fatalf("actual runtime Git scan:%d %s", status, data)
		}
		post(base, domain.Command{Action: "create_github_connection", ID: "tls-conn", ProductID: "smoke-p", Data: map[string]any{"name": "private TLS fixture", "app_id": 1, "installation_id": 2, "private_key_ref": "env:RCP_FIXTURE_APP_KEY", "owner": "fixture", "api_url": "https://tls-fixture:8443"}})
		// Go sends alertBadCertificate after x509 chain verification failure.
		// The public API intentionally exposes only its sanitized provider error.
		beforeAlerts := strings.Count(docker("logs", clientID), "SSLV3_ALERT_BAD_CERTIFICATE")
		status, data = request(base, "POST", "/api/connections/tls-conn/test", []byte(`{}`), true)
		if mode == "augmented" {
			if status != 200 {
				t.Fatalf("actual Go provider rejected private positive TLS fixture:%d", status)
			}
		} else {
			var result struct {
				Error string `json:"error"`
			}
			json.Unmarshal(data, &result)
			if status != 422 || result.Error != "validation: connection test: provider request failed or timed out" || strings.Count(docker("logs", clientID), "SSLV3_ALERT_BAD_CERTIFICATE") != beforeAlerts+1 {
				t.Fatalf("actual Go provider did not demonstrate certificate rejection: status=%d body=%s", status, data)
			}
		}
		post(base, domain.Command{Action: "delete_product", ProductID: "smoke-p", Data: map[string]any{"name": "private runtime fixture"}})
		for _, path := range []string{"/api/products/smoke-p/context", "/api/features/smoke-f/context"} {
			if status, _ := request(base, "GET", path, nil, true); status != 404 {
				t.Fatalf("deleted runtime object remains accessible: %s status%d", path, status)
			}
		}
		t.Logf("admitted runtime digest=%s public_ca_sha256=%s TLS_mode=%s provider_status=%d Git/supported_API/asset=PASS CA_injection=false", artifact.Digest, expectedHash, mode, status)
	}
}
