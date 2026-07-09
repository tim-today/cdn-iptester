package geoipdata

import (
	_ "embed"
	"os"
	"path/filepath"
)

const Filename = "GeoLite2-Country.mmdb"

//go:embed GeoLite2-Country.mmdb
var countryDB []byte

func EnsureCountryDB(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	path := filepath.Join(dir, Filename)
	if fileExists(path) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, countryDB, 0644); err != nil {
		return "", err
	}
	return path, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
