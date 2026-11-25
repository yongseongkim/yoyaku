package model

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Artifact struct {
	URL    string `json:"url"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}
type RuntimePins struct {
	LlamaVersion   string              `json:"llama_version"`
	WhisperVersion string              `json:"whisper_version"`
	Llama          map[string]Artifact `json:"llama"`
	Whisper        Artifact            `json:"whisper"`
}

func ReadRuntimePins(root string) (RuntimePins, error) {
	var p RuntimePins
	data, e := os.ReadFile(filepath.Join(root, "native-runtimes.json"))
	if e != nil {
		return p, e
	}
	e = json.Unmarshal(data, &p)
	return p, e
}
func Hash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}

// Download는 서버가 요청한 위치부터 데이터를 보내는지 확인한 뒤 다운로드를 이어 간다.
// 파일 전체를 검증한 뒤에만 설치 경로로 옮긴다.
func Download(ctx context.Context, url, path, digest string, size int64, progress io.Writer) error {
	if sum, e := Hash(path); e == nil && sum == digest {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	partial := path + ".part"
	offset := int64(0)
	if info, e := os.Stat(partial); e == nil {
		offset = info.Size()
	}
	if size > 0 && offset > size {
		if e := os.Remove(partial); e != nil {
			return e
		}
		offset = 0
	}
	if size == 0 || offset < size {
		req, e := http.NewRequestWithContext(ctx, "GET", url, nil)
		if e != nil {
			return e
		}
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 45 * time.Second}}
		resp, e := client.Do(req)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			return fmt.Errorf("download HTTP %d", resp.StatusCode)
		}
		if offset > 0 && resp.StatusCode == 206 {
			if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)) {
				return errors.New("Incorrect resume range")
			}
		} else {
			offset = 0
		}
		flags := os.O_CREATE | os.O_WRONLY
		if offset == 0 {
			flags |= os.O_TRUNC
		}
		f, e := os.OpenFile(partial, flags, 0600)
		if e != nil {
			return e
		}
		if _, e = f.Seek(offset, 0); e != nil {
			f.Close()
			return e
		}
		counter := &downloadProgress{out: progress, bytes: offset, total: size, last: time.Now()}
		_, e = io.Copy(f, io.TeeReader(resp.Body, counter))
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	info, e := os.Stat(partial)
	if e != nil {
		return e
	}
	if size > 0 && info.Size() != size {
		return errors.New("Downloaded size differs from pin")
	}
	if progress != nil {
		fmt.Fprintln(progress, "Verifying SHA-256:", filepath.Base(path))
	}
	sum, e := Hash(partial)
	if e != nil {
		return e
	}
	if sum != digest {
		_ = os.Remove(partial)
		return errors.New("SHA-256 differs from pinned artifact")
	}
	return os.Rename(partial, path)
}

type downloadProgress struct {
	out          io.Writer
	bytes, total int64
	last         time.Time
}

func (p *downloadProgress) Write(data []byte) (int, error) {
	p.bytes += int64(len(data))
	if p.out != nil && time.Since(p.last) > 5*time.Second {
		fmt.Fprintf(p.out, "Downloaded %.0f / %.0f MiB\n", float64(p.bytes)/1048576, float64(p.total)/1048576)
		p.last = time.Now()
	}
	return len(data), nil
}
func InstallModel(ctx context.Context, root, id string, p Profile, out io.Writer) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Installed(root, id) {
		return nil
	}
	if err := Download(ctx, p.URL(), p.Path(root, id), p.SHA256, p.Size, out); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	return atomicWrite(p.Path(root, id)+".verified.json", data)
}
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
func InstallRuntimes(ctx context.Context, root string, out io.Writer) error {
	pins, err := ReadRuntimePins(root)
	if err != nil {
		return err
	}
	artifact, ok := pins.Llama[runtime.GOOS+"-"+runtime.GOARCH]
	if !ok {
		return errors.New("Pinned native runtime currently supports macOS arm64/amd64")
	}
	for _, item := range []struct {
		artifact Artifact
		name     string
		zip      bool
	}{{artifact, "llama", false}, {pins.Whisper, "whisper", true}} {
		dir := filepath.Join(root, ".local-native", item.name)
		seal := filepath.Join(dir, ".verified")
		if b, e := os.ReadFile(seal); e == nil && string(b) == item.artifact.SHA256 {
			continue
		}
		archive := filepath.Join(root, ".local-native/downloads", item.artifact.File)
		if err = Download(ctx, item.artifact.URL, archive, item.artifact.SHA256, 0, out); err != nil {
			return err
		}
		stage, err := os.MkdirTemp(filepath.Join(root, ".local-native"), ".runtime-*")
		if err != nil {
			return err
		}
		if item.zip {
			err = unzip(archive, stage, "build-apple/whisper.xcframework/macos-arm64_x86_64/")
		} else {
			err = untar(archive, stage)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(stage, ".verified"), []byte(item.artifact.SHA256), 0644)
		}
		if err == nil {
			old := dir + ".old"
			_ = os.RemoveAll(old)
			if _, e := os.Stat(dir); e == nil {
				err = os.Rename(dir, old)
			}
			if err == nil {
				err = os.Rename(stage, dir)
			}
			if err == nil {
				_ = os.RemoveAll(old)
			}
		}
		_ = os.RemoveAll(stage)
		if err != nil {
			return err
		}
	}
	return nil
}
func safePath(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("Unsafe archive entry")
	}
	return filepath.Join(root, clean), nil
}
func unzip(archive, dir, prefix string) error {
	z, e := zip.OpenReader(archive)
	if e != nil {
		return e
	}
	defer z.Close()
	for _, f := range z.File {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		name := strings.TrimPrefix(f.Name, prefix)
		if name == "" {
			continue
		}
		target, e := safePath(dir, name)
		if e != nil {
			return e
		}
		if f.FileInfo().IsDir() {
			if e = os.MkdirAll(target, 0755); e != nil {
				return e
			}
			continue
		}
		if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
			return e
		}
		reader, e := f.Open()
		if e != nil {
			return e
		}
		if f.Mode()&os.ModeSymlink != 0 {
			b, e := io.ReadAll(io.LimitReader(reader, 4096))
			reader.Close()
			if e != nil {
				return e
			}
			link := string(b)
			if _, e = safePath(dir, filepath.Join(filepath.Dir(name), link)); e != nil {
				return e
			}
			if e = os.Symlink(link, target); e != nil {
				return e
			}
			continue
		}
		out, e := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode().Perm())
		if e != nil {
			reader.Close()
			return e
		}
		_, e = io.Copy(out, io.LimitReader(reader, 128*1024*1024))
		reader.Close()
		out.Close()
		if e != nil {
			return e
		}
	}
	return nil
}
func untar(archive, dir string) error {
	f, e := os.Open(archive)
	if e != nil {
		return e
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, e := tr.Next()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		parts := strings.SplitN(header.Name, "/", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		name := parts[1]
		target, e := safePath(dir, name)
		if e != nil {
			return e
		}
		switch header.Typeflag {
		case tar.TypeDir:
			e = os.MkdirAll(target, 0755)
		case tar.TypeSymlink:
			if _, e = safePath(dir, filepath.Join(filepath.Dir(name), header.Linkname)); e != nil {
				return e
			}
			if e = os.MkdirAll(filepath.Dir(target), 0755); e == nil {
				e = os.Symlink(header.Linkname, target)
			}
		case tar.TypeReg:
			if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
				return e
			}
			var out *os.File
			out, e = os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0777)
			if e == nil {
				_, e = io.Copy(out, tr)
				out.Close()
			}
		default:
			return errors.New("Unsupported native archive entry " + strconv.Itoa(int(header.Typeflag)))
		}
		if e != nil {
			return e
		}
	}
}
