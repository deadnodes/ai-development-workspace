package workerrunner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
)

func digestBytes(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}
type imageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}
type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

// VerifyOCI verifies descriptor closure, platform and diffIDs before reporting a
// local-only assertion. It rejects unattached blobs and multiple images.
func VerifyOCI(file string, max int64, platform string) (domain.WorkerLocalOCI, error) {
	return verifyOCI(file, max, platform, nil, nil)
}
func verifyOCI(file string, max int64, platform string, execution *domain.WorkerExecution, compiled []byte) (domain.WorkerLocalOCI, error) {
	var out domain.WorkerLocalOCI
	f, e := os.Open(file)
	if e != nil {
		return out, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(b)) > max || len(b) == 0 {
		return out, errors.New("OCI archive outside bounds")
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	total := int64(0)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
		name := strings.TrimSuffix(h.Name, "/")
		if h.Typeflag == tar.TypeDir {
			if name != "blobs" && name != "blobs/sha256" {
				return out, errors.New("unexpected OCI directory")
			}
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || h.Size < 0 || h.Size > max || files[name] != nil {
			return out, errors.New("invalid OCI entry")
		}
		if name != "oci-layout" && name != "index.json" && !(strings.HasPrefix(name, "blobs/sha256/") && len(name) == 77) {
			return out, errors.New("unexpected OCI path")
		}
		total += h.Size
		if total > max {
			return out, errors.New("OCI expansion exceeds bound")
		}
		data, e := io.ReadAll(io.LimitReader(tr, max+1))
		if e != nil || int64(len(data)) != h.Size {
			return out, errors.New("invalid OCI entry size")
		}
		if strings.HasPrefix(name, "blobs/") && digestBytes(data) != "sha256:"+strings.TrimPrefix(name, "blobs/sha256/") {
			return out, errors.New("OCI blob digest mismatch")
		}
		files[name] = data
	}
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	if json.Unmarshal(files["oci-layout"], &layout) != nil || layout.Version != "1.0.0" {
		return out, errors.New("invalid OCI layout")
	}
	var index imageIndex
	if json.Unmarshal(files["index.json"], &index) != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return out, errors.New("exactly one OCI image required")
	}
	visited := map[string]bool{}
	blob := func(d descriptor) ([]byte, error) {
		if !strings.HasPrefix(d.Digest, "sha256:") || len(d.Digest) != 71 {
			return nil, errors.New("invalid descriptor digest")
		}
		data, ok := files["blobs/sha256/"+strings.TrimPrefix(d.Digest, "sha256:")]
		if !ok || int64(len(data)) != d.Size || digestBytes(data) != d.Digest {
			return nil, errors.New("OCI descriptor mismatch")
		}
		visited[d.Digest] = true
		return data, nil
	}
	md := index.Manifests[0]
	if md.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		return out, errors.New("OCI manifest media type required")
	}
	manifestBytes, e := blob(md)
	if e != nil {
		return out, e
	}
	var m imageManifest
	if json.Unmarshal(manifestBytes, &m) != nil || m.SchemaVersion != 2 || m.Config.MediaType != "application/vnd.oci.image.config.v1+json" || len(m.Layers) != 1 {
		return out, errors.New("unsupported OCI manifest")
	}
	config, e := blob(m.Config)
	if e != nil {
		return out, e
	}
	var c struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Config       struct {
			User       string   `json:"User"`
			Entrypoint []string `json:"Entrypoint"`
		} `json:"config"`
		RootFS struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if json.Unmarshal(config, &c) != nil || c.OS+"/"+c.Architecture != platform || c.RootFS.Type != "layers" || len(c.RootFS.DiffIDs) != 1 {
		return out, errors.New("OCI platform or rootfs mismatch")
	}
	if md.Platform != nil && md.Platform.OS+"/"+md.Platform.Architecture != platform {
		return out, errors.New("index platform mismatch")
	}
	layer, e := blob(m.Layers[0])
	if e != nil {
		return out, e
	}
	var decompressed []byte
	switch m.Layers[0].MediaType {
	case "application/vnd.oci.image.layer.v1.tar":
		decompressed = layer
	case "application/vnd.oci.image.layer.v1.tar+gzip":
		gz, e := gzip.NewReader(bytes.NewReader(layer))
		if e != nil {
			return out, e
		}
		decompressed, e = io.ReadAll(io.LimitReader(gz, max+1))
		_ = gz.Close()
		if e != nil || int64(len(decompressed)) > max {
			return out, errors.New("layer expansion exceeds bound")
		}
	default:
		return out, errors.New("unsupported OCI layer")
	}
	if digestBytes(decompressed) != c.RootFS.DiffIDs[0] {
		return out, errors.New("OCI layer diffID mismatch")
	}
	if execution != nil {
		if c.Config.User != "65532:65532" || !reflect.DeepEqual(c.Config.Entrypoint, execution.Entrypoint) {
			return out, errors.New("OCI runtime contract mismatch")
		}
		layerTar := tar.NewReader(bytes.NewReader(decompressed))
		found := false
		for {
			h, err := layerTar.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return out, err
			}
			if h.Typeflag == tar.TypeDir {
				continue
			}
			if found || h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || h.Name != execution.ImagePath || h.Mode&0777 != 0555 || h.Size != int64(len(compiled)) {
				return out, errors.New("OCI layer differs from compiled output contract")
			}
			data, err := io.ReadAll(io.LimitReader(layerTar, max+1))
			if err != nil || !bytes.Equal(data, compiled) {
				return out, errors.New("OCI compiled output identity mismatch")
			}
			found = true
		}
		if !found {
			return out, errors.New("compiled output missing from OCI layer")
		}
	}
	for name := range files {
		if strings.HasPrefix(name, "blobs/") && !visited["sha256:"+strings.TrimPrefix(name, "blobs/sha256/")] {
			return out, errors.New("unreferenced OCI blob")
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var identity bytes.Buffer
	for _, name := range names {
		identity.WriteString(name + "\x00" + digestBytes(files[name]) + "\n")
	}
	out.ArchiveHash = digestBytes(b)
	out.LayoutHash = digestBytes(identity.Bytes())
	out.IndexDigest = digestBytes(files["index.json"])
	out.ManifestDigest = md.Digest
	out.ArchiveBytes = int64(len(b))
	out.Platform = platform
	return out, nil
}
