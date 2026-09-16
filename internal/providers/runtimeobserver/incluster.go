package runtimeobserver

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
)

const serviceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"

// Explicit configuration never silently falls back to another cluster.
func resolveAuthentication(t *Target, getenv func(string) string, directory string) (kubeMaterial, error) {
	explicit := t.Kubeconfig != "" || t.Context != "" || t.APIURL != "" || t.TokenFile != "" || t.CAFile != "" || t.ClientCertFile != "" || t.ClientKeyFile != ""
	if t.InCluster && explicit {
		return kubeMaterial{}, fmt.Errorf("in_cluster cannot be combined with kubeconfig or explicit API credentials")
	}
	if !t.InCluster {
		if t.Kubeconfig != "" || t.Context != "" {
			return resolveKubeconfig(t)
		}
		if t.APIURL != "" {
			return kubeMaterial{}, nil
		}
		if t.TokenFile != "" || t.CAFile != "" || t.ClientCertFile != "" || t.ClientKeyFile != "" {
			return kubeMaterial{}, fmt.Errorf("explicit credentials require api_url")
		}
		if getenv("KUBECONFIG") != "" {
			return resolveKubeconfig(t)
		}
		if getenv("KUBERNETES_SERVICE_HOST") == "" && getenv("KUBERNETES_SERVICE_PORT") == "" {
			return resolveKubeconfig(t)
		}
	}
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	n, err := strconv.Atoi(port)
	if net.ParseIP(host) == nil || err != nil || n < 1 || n > 65535 {
		return kubeMaterial{}, fmt.Errorf("in-cluster Kubernetes service host/port unavailable or invalid")
	}
	t.APIURL = "https://" + net.JoinHostPort(host, port)
	t.CAFile = filepath.Join(directory, "ca.crt")
	t.TokenFile = filepath.Join(directory, "token")
	t.Context = "in-cluster"
	// get reads the projected token file on each request to honor kubelet rotation.
	return kubeMaterial{}, nil
}
