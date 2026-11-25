package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

type Profile struct {
	Name                string `json:"name"`
	Directory           string `json:"directory"`
	Repository          string `json:"repository"`
	Revision            string `json:"revision"`
	File                string `json:"file"`
	SHA256              string `json:"sha256"`
	Size                int64  `json:"size"`
	TranslationOnly     bool   `json:"translation_only,omitempty"`
	TranslationTemplate string `json:"translation_template,omitempty"`
}
type Catalog struct {
	Default  string             `json:"default"`
	Profiles map[string]Profile `json:"profiles"`
	Speech   Profile            `json:"speech"`
}

func Read(root string) (Catalog, error) {
	var c Catalog
	data, err := os.ReadFile(filepath.Join(root, "models.json"))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	if err != nil {
		return c, err
	}
	if _, ok := c.Profiles[c.Default]; !ok {
		return c, errors.New("Invalid default model")
	}
	for id, p := range c.Profiles {
		if !regexp.MustCompile(`^[a-z0-9_-]+$`).MatchString(id) {
			return c, errors.New("Invalid profile ID")
		}
		if err := p.Validate(); err != nil {
			return c, fmt.Errorf("%s: %w", id, err)
		}
	}
	if err := c.Speech.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
func (p Profile) Validate() error {
	if p.TranslationTemplate != "" && (!p.TranslationOnly || p.TranslationTemplate != "hy-mt1.5") {
		return errors.New("Unsupported translation template or incompatible model role")
	}
	if p.Name == "" || p.Directory == "" || p.Directory == "." || p.Directory == ".." || p.File == "." || p.File == ".." || p.File == "" || filepath.Base(p.Directory) != p.Directory || filepath.Base(p.File) != p.File || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(p.Revision) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.SHA256) || p.Size <= 0 || !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(p.Repository) {
		return errors.New("Invalid pinned model profile")
	}
	return nil
}
func (p Profile) URL() string {
	return "https://huggingface.co/" + p.Repository + "/resolve/" + p.Revision + "/" + p.File
}
func (p Profile) Path(root, id string) string {
	return filepath.Join(root, ".local-native/models", id, p.File)
}
func (p Profile) Installed(root, id string) bool {
	info, err := os.Stat(p.Path(root, id))
	if err != nil || info.Size() != p.Size {
		return false
	}
	data, err := os.ReadFile(p.Path(root, id) + ".verified.json")
	if err != nil {
		return false
	}
	var saved Profile
	if json.Unmarshal(data, &saved) != nil {
		return false
	}
	return saved == p
}
