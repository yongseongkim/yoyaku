package research

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyPublicRecordWithoutLocalArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "benchmark"), 0700); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(root, "benchmark", "README.md")
	if err := os.WriteFile(index, []byte("[모델 비교](models.md)"), 0600); err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	if err := Verify(root, &result); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing category page: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "benchmark", "models.md"), []byte("# 모델 비교"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, &result); err != nil {
		t.Fatal(err)
	}
}

func TestFailedTranslationHasNoSemanticGrade(t *testing.T) {
	root := t.TempDir()
	report := []byte(`{"profile":"test","execution_complete":true,"suite_hash":"s","model":{"revision":"r"},"rows":[{"id":"one","task":"translation","status":"error"}]}`)
	if err := os.WriteFile(filepath.Join(root, "report.json"), report, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "review.json")
	for _, grade := range []string{"unavailable", "faithful", "meaning_error", "uncertain"} {
		data := fmt.Sprintf(`{"suite_hash":"s","models":[{"profile":"test","revision":"r","report":"report.json","report_sha256":"%x","judgments":[{"id":"one","grade":"%s","reason":"worker error"}]}]}`, sha256.Sum256(report), grade)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		n, err := verifyQualityAt(os.DirFS(root), "review.json")
		if n != 0 || (err == nil) != (grade == "unavailable") {
			t.Fatalf("grade %s, count %d, error %v", grade, n, err)
		}
	}
}
