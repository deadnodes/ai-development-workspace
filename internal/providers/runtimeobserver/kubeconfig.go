package runtimeobserver

import (
	"encoding/base64"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
)

type kubeCluster struct {
	Server     string `yaml:"server"`
	CA         string `yaml:"certificate-authority"`
	CAData     string `yaml:"certificate-authority-data"`
	Insecure   bool   `yaml:"insecure-skip-tls-verify"`
	ServerName string `yaml:"tls-server-name"`
	Proxy      string `yaml:"proxy-url"`
}
type kubeUser struct {
	Token        string `yaml:"token"`
	TokenFile    string `yaml:"tokenFile"`
	Cert         string `yaml:"client-certificate"`
	CertData     string `yaml:"client-certificate-data"`
	Key          string `yaml:"client-key"`
	KeyData      string `yaml:"client-key-data"`
	Exec         any    `yaml:"exec"`
	AuthProvider any    `yaml:"auth-provider"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
}
type kubeContext struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}
type kubeDocument struct {
	Clusters []struct {
		Name    string      `yaml:"name"`
		Cluster kubeCluster `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string   `yaml:"name"`
		User kubeUser `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string      `yaml:"name"`
		Context kubeContext `yaml:"context"`
	} `yaml:"contexts"`
}
type kubeMaterial struct {
	ca, cert, key     []byte
	token, serverName string
}

func resolveKubeconfig(t *Target) (kubeMaterial, error) {
	var m kubeMaterial
	if strings.TrimSpace(t.Context) == "" {
		return m, fmt.Errorf("explicit Kubernetes context required")
	}
	path := t.Kubeconfig
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return m, fmt.Errorf("kubeconfig home unavailable")
		}
		path = filepath.Join(home, ".kube", "config")
	}
	clusters := map[string]kubeCluster{}
	users := map[string]kubeUser{}
	contexts := map[string]kubeContext{}
	paths := filepath.SplitList(path)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		}
		b, e := os.ReadFile(p)
		if e != nil {
			if os.IsNotExist(e) && len(paths) > 1 {
				continue
			}
			return m, fmt.Errorf("kubeconfig file unavailable")
		}
		var d kubeDocument
		if yaml.Unmarshal(b, &d) != nil {
			return m, fmt.Errorf("invalid kubeconfig document")
		}
		local := func(s string) string {
			if s != "" && !filepath.IsAbs(s) {
				return filepath.Join(filepath.Dir(p), s)
			}
			return s
		}
		for _, x := range d.Clusters {
			if _, ok := clusters[x.Name]; !ok {
				x.Cluster.CA = local(x.Cluster.CA)
				clusters[x.Name] = x.Cluster
			}
		}
		for _, x := range d.Users {
			if _, ok := users[x.Name]; !ok {
				x.User.Cert = local(x.User.Cert)
				x.User.Key = local(x.User.Key)
				x.User.TokenFile = local(x.User.TokenFile)
				users[x.Name] = x.User
			}
		}
		for _, x := range d.Contexts {
			if _, ok := contexts[x.Name]; !ok {
				contexts[x.Name] = x.Context
			}
		}
	}
	c, ok := contexts[t.Context]
	if !ok {
		return m, fmt.Errorf("configured Kubernetes context not found")
	}
	cluster, ok := clusters[c.Cluster]
	if !ok {
		return m, fmt.Errorf("kubeconfig cluster not found")
	}
	user := users[c.User]
	if c.User != "" {
		if _, ok := users[c.User]; !ok {
			return m, fmt.Errorf("kubeconfig user not found")
		}
	}
	if cluster.Insecure || cluster.Proxy != "" {
		return m, fmt.Errorf("insecure TLS and proxy-url are unsupported for runtime observation")
	}
	if user.Exec != nil || user.AuthProvider != nil || user.Username != "" || user.Password != "" {
		return m, fmt.Errorf("kubeconfig exec, auth-provider and basic authentication are unsupported; use certificate or token credentials")
	}
	decode := func(s string) ([]byte, error) {
		if s == "" {
			return nil, nil
		}
		b, e := base64.StdEncoding.DecodeString(s)
		if e != nil {
			return nil, fmt.Errorf("invalid kubeconfig certificate data")
		}
		return b, nil
	}
	var e error
	if m.ca, e = decode(cluster.CAData); e != nil {
		return m, e
	}
	if m.cert, e = decode(user.CertData); e != nil {
		return m, e
	}
	if m.key, e = decode(user.KeyData); e != nil {
		return m, e
	}
	t.APIURL = cluster.Server
	t.CAFile = cluster.CA
	t.ClientCertFile = user.Cert
	t.ClientKeyFile = user.Key
	t.TokenFile = user.TokenFile
	m.token = user.Token
	m.serverName = cluster.ServerName
	return m, nil
}
