package steamcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stageConfigFiles copies files into install_dir after install/update and
// just before launch. It exists because Nomad renders `template` blocks
// before the driver starts -- i.e. before install_dir has any content and
// before volume_mount symlinks exist -- so a template cannot write directly
// into the install tree. Instead the template renders to somewhere
// task-local (e.g. local/server.json) and config_files copies it in.
//
// files maps source -> destination. Sources resolve against taskDir when
// relative. Destinations resolve against installDir and must stay inside it.
// Existing destinations are overwritten on every start (declarative).
func stageConfigFiles(taskDir, installDir string, files map[string]string) error {
	srcs := make([]string, 0, len(files))
	for s := range files {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)

	for _, src := range srcs {
		dst := files[src]
		srcPath := src
		if !filepath.IsAbs(srcPath) {
			srcPath = filepath.Join(taskDir, srcPath)
		}
		dstPath, err := stageDestination(installDir, dst)
		if err != nil {
			return err
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("config_files %q -> %q: %w", src, dst, err)
		}
	}
	return nil
}

// mergeConfigFiles flattens the config_files list-of-maps (see the schema
// note in config.go) into one source -> destination map. Later entries win.
func mergeConfigFiles(in []map[string]string) map[string]string {
	out := make(map[string]string)
	for _, m := range in {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// stageDestination resolves a config_files destination inside installDir.
func stageDestination(installDir, dst string) (string, error) {
	if dst == "" || filepath.IsAbs(dst) {
		return "", fmt.Errorf("config_files destination %q must be a non-empty path relative to install_dir", dst)
	}
	p := filepath.Join(installDir, dst)
	rel, err := filepath.Rel(installDir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("config_files destination %q escapes install_dir", dst)
	}
	return p, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Keep the source's permissions (a 0600 secret must not become 0644, a
	// script must keep its exec bit).
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Write via a temp file so a game never sees a half-written config.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".stage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, st.Mode().Perm()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, dst)
}
