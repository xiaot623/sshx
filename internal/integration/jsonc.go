package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tailscale/hujson"
)

func StringProperty(src []byte, key string) (string, bool, error) {
	v, err := parseObject(src)
	if err != nil {
		return "", false, err
	}
	found := v.Find(jsonPointer(key))
	if found == nil {
		return "", false, nil
	}
	std := found.Clone()
	std.Standardize()
	var value string
	if err := json.Unmarshal(std.Pack(), &value); err != nil {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return value, true, nil
}

func SetStringProperty(src []byte, key, value string) ([]byte, error) {
	v, err := parseObject(src)
	if err != nil {
		return nil, err
	}
	patch, err := json.Marshal([]map[string]any{{
		"op":    "add",
		"path":  jsonPointer(key),
		"value": value,
	}})
	if err != nil {
		return nil, err
	}
	if err := v.Patch(patch); err != nil {
		return nil, err
	}
	return v.Pack(), nil
}

func parseObject(src []byte) (hujson.Value, error) {
	v, err := hujson.Parse(src)
	if err != nil {
		return hujson.Value{}, err
	}
	if v.Value.Kind() != '{' {
		return hujson.Value{}, errors.New("settings root must be a JSON object")
	}
	return v, nil
}

// jsonPointer treats key as a single object member name, including dots.
func jsonPointer(key string) string {
	escaped := strings.ReplaceAll(key, "~", "~0")
	escaped = strings.ReplaceAll(escaped, "/", "~1")
	return "/" + escaped
}
