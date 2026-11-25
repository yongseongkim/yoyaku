package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalKey는 서버에서 로컬 모델을 호출할 때 쓸 키를 읽거나 처음 한 번 생성한다.
// 여러 프로세스가 함께 시작해도 먼저 생성한 키 파일을 공유한다.
func LocalKey(root string) (string, error) {
	path := filepath.Join(root, ".local-native/worker.key")
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return "", e
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	key := hex.EncodeToString(b)
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e == nil {
		_, e = f.WriteString(key + "\n")
		ce := f.Close()
		if e != nil {
			return "", e
		}
		return key, ce
	}
	if !os.IsExist(e) {
		return "", e
	}
	for i := 0; i < 20; i++ {
		data, e := os.ReadFile(path)
		if e != nil {
			return "", e
		}
		key = strings.TrimSpace(string(data))
		if len(key) == 64 {
			if _, e = hex.DecodeString(key); e == nil {
				return key, nil
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", errors.New("Invalid local worker key")
}
