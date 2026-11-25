// research 패키지는 모델을 다시 실행하지 않고 벤치마크 문서와 보관된 원본을 점검한다.
package research

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
)

const localArchive = ".artifacts/benchmark-evidence/originals.zip"

var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)

func Verify(root string, out io.Writer) error {
	files := os.DirFS(root)
	pages, err := fs.Glob(files, "benchmark/*.md")
	if err != nil {
		return err
	}
	if _, err := fs.Stat(files, "benchmark/README.md"); err != nil {
		return err
	}
	for _, page := range pages {
		doc, err := fs.ReadFile(files, page)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllSubmatch(doc, -1) {
			link, err := url.Parse(string(match[1]))
			if err != nil {
				return fmt.Errorf("%s: %w", page, err)
			}
			if link.IsAbs() || link.Host != "" || link.Path == "" {
				continue
			}
			if _, err := fs.Stat(files, path.Join(path.Dir(page), link.Path)); err != nil {
				return fmt.Errorf("%s: %w", page, err)
			}
		}
	}
	fmt.Fprintf(out, "벤치마크 문서 %d개의 로컬 파일 링크를 확인했습니다.\n", len(pages))
	data, err := fs.ReadFile(files, localArchive)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(out, "이 작업 폴더에는 이전 실험의 로컬 원본이 없어 원본 검증은 생략했습니다.")
		return nil
	}
	if err != nil {
		return err
	}
	digest, err := fs.ReadFile(files, localArchive+".sha256")
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != strings.TrimSpace(string(digest)) {
		return errors.New("benchmark evidence ZIP checksum mismatch")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	return verifyArchive(archive, out)
}

func verifyArchive(files fs.FS, out io.Writer) error {
	manifests, e := fs.Glob(files, "benchmarks/local-llm/*/manifest.json")
	if e != nil {
		return e
	}
	if len(manifests) == 0 {
		return errors.New("No benchmark manifests found")
	}
	count, n := 0, 0
	for _, manifest := range manifests {
		dir := path.Dir(manifest)
		data, e := fs.ReadFile(files, manifest)
		if e != nil {
			return e
		}
		var m struct {
			Files map[string]string `json:"files"`
		}
		if e = json.Unmarshal(data, &m); e != nil {
			return e
		}
		if len(m.Files) == 0 {
			return fmt.Errorf("Empty archive manifest: %s", manifest)
		}
		for name, want := range m.Files {
			if !fs.ValidPath(name) {
				return errors.New("Unsafe archive path")
			}
			data, e := fs.ReadFile(files, path.Join(dir, name))
			if e != nil {
				return e
			}
			h := sha256.Sum256(data)
			if hex.EncodeToString(h[:]) != want {
				return fmt.Errorf("Historical evidence differs: %s/%s", path.Base(dir), name)
			}
			count++
		}
		review := path.Join(dir, "translation-quality-review.json")
		if _, err := fs.Stat(files, review); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		extra, err := verifyQualityAt(files, review)
		if err != nil {
			return err
		}
		n += extra
	}
	fmt.Fprintf(out, "Verified %d archived files across %d manifests; %d recorded translation judgments remain tied to their original reports.\n", count, len(manifests), n)
	return nil
}

func verifyQualityAt(files fs.FS, reviewPath string) (int, error) {
	var review struct {
		SuiteHash string `json:"suite_hash"`
		Models    []struct {
			Profile, Revision, Report string
			Digest                    string `json:"report_sha256"`
			Judgments                 []struct{ ID, Grade, Reason string }
		}
	}
	data, e := fs.ReadFile(files, reviewPath)
	if e != nil {
		return 0, e
	}
	if e = json.Unmarshal(data, &review); e != nil {
		return 0, e
	}
	if review.SuiteHash == "" || len(review.Models) == 0 {
		return 0, errors.New("Empty quality provenance")
	}
	total := 0
	profiles := map[string]bool{}
	for _, m := range review.Models {
		if profiles[m.Profile] {
			return 0, errors.New("Duplicate model review")
		}
		profiles[m.Profile] = true
		if !fs.ValidPath(m.Report) {
			return 0, errors.New("Unsafe quality report path")
		}
		data, e := fs.ReadFile(files, m.Report)
		if e != nil {
			return 0, e
		}
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) != m.Digest {
			return 0, errors.New("Report changed; a new quality review is required")
		}
		var report struct {
			Profile   string
			Complete  bool   `json:"execution_complete"`
			SuiteHash string `json:"suite_hash"`
			Model     struct{ Revision string }
			Rows      []struct{ ID, Task, Status string }
		}
		if json.Unmarshal(data, &report) != nil || !report.Complete || report.Profile != m.Profile || report.Model.Revision != m.Revision || report.SuiteHash != review.SuiteHash {
			return 0, errors.New("Quality review provenance mismatch")
		}
		status := map[string]string{}
		for _, r := range report.Rows {
			if r.Task == "translation" {
				if _, found := status[r.ID]; found {
					return 0, errors.New("Duplicate report row")
				}
				status[r.ID] = r.Status
			}
		}
		seen := map[string]bool{}
		for _, j := range m.Judgments {
			s, found := status[j.ID]
			if seen[j.ID] || !found || strings.TrimSpace(j.Reason) == "" {
				return 0, errors.New("Missing, unexpected, or duplicate judgment")
			}
			switch j.Grade {
			case "faithful", "minor", "meaning_error", "uncertain":
				if s == "error" {
					return 0, errors.New("Cannot grade a failed translation")
				}
				total++
			case "unavailable":
				if s != "error" {
					return 0, errors.New("Only failed translations can be marked unavailable")
				}
			default:
				return 0, errors.New("Unknown quality grade")
			}
			seen[j.ID] = true
		}
		if len(seen) != len(status) {
			return 0, errors.New("Incomplete quality review")
		}
	}
	return total, nil
}
