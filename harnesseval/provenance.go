package harnesseval

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/websines/structor-harness/record"
)

type Executable struct {
	Digest   record.Digest `json:"digest"`
	Go       string        `json:"go"`
	Platform string        `json:"platform"`
	Revision string        `json:"revision,omitempty"`
	Dirty    bool          `json:"dirty"`
}

func executableProvenance() (Executable, error) {
	p, err := os.Executable()
	if err != nil {
		return Executable{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return Executable{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return Executable{}, err
	}
	e := Executable{Digest: record.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				e.Revision = setting.Value
			case "vcs.modified":
				e.Dirty = setting.Value == "true"
			}
		}
	}
	return e, nil
}
