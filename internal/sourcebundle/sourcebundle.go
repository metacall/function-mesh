package sourcebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const IndexKey = "source-index.json"

var configMapKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Encode converts logical, nested source paths into ConfigMap-safe keys and
// stores the key-to-path mapping in a reserved index entry.
func Encode(files map[string]string) (map[string]string, error) {
	logicalPaths := make([]string, 0, len(files))
	for logicalPath := range files {
		logicalPaths = append(logicalPaths, logicalPath)
	}
	sort.Strings(logicalPaths)

	data := make(map[string]string, len(files)+1)
	index := make(map[string]string, len(files))
	for _, rawPath := range logicalPaths {
		normalized, err := NormalizePath(rawPath)
		if err != nil {
			return nil, fmt.Errorf("source file %q: %w", rawPath, err)
		}
		key := configMapKey(normalized)
		if _, duplicate := data[key]; duplicate {
			return nil, fmt.Errorf("source files produce duplicate ConfigMap key %q", key)
		}
		data[key] = files[rawPath]
		index[key] = normalized
	}
	encodedIndex, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	data[IndexKey] = string(encodedIndex)
	return data, nil
}

// VolumeItems decodes an indexed source ConfigMap into nested volume items.
// ConfigMaps without an index are legacy flat bundles and return nil, false.
func VolumeItems(data map[string]string) ([]corev1.KeyToPath, bool, error) {
	rawIndex, indexed := data[IndexKey]
	if !indexed {
		return nil, false, nil
	}
	var index map[string]string
	if err := json.Unmarshal([]byte(rawIndex), &index); err != nil {
		return nil, true, fmt.Errorf("decode %s: %w", IndexKey, err)
	}
	items := make([]corev1.KeyToPath, 0, len(index))
	seenPaths := make(map[string]struct{}, len(index))
	for key, rawPath := range index {
		if key == IndexKey {
			return nil, true, fmt.Errorf("source index references reserved key %q", key)
		}
		if _, exists := data[key]; !exists {
			return nil, true, fmt.Errorf("source index references missing key %q", key)
		}
		normalized, err := NormalizePath(rawPath)
		if err != nil {
			return nil, true, fmt.Errorf("source index key %q: %w", key, err)
		}
		if _, duplicate := seenPaths[normalized]; duplicate {
			return nil, true, fmt.Errorf("source index contains duplicate path %q", normalized)
		}
		seenPaths[normalized] = struct{}{}
		items = append(items, corev1.KeyToPath{Key: key, Path: normalized})
	}
	if len(index) != len(data)-1 {
		return nil, true, fmt.Errorf("source index does not describe every data key")
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, true, nil
}

func NormalizePath(rawPath string) (string, error) {
	normalized := path.Clean(strings.ReplaceAll(rawPath, `\`, "/"))
	if normalized == "." || normalized == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(normalized, "/") || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("path must remain relative")
	}
	return normalized, nil
}

func EstimateSize(data map[string]string) int {
	total := 0
	for key, value := range data {
		total += len(key) + len(value)
	}
	return total
}

func configMapKey(logicalPath string) string {
	if logicalPath != IndexKey && len(logicalPath) <= 253 && !strings.Contains(logicalPath, "/") && configMapKeyPattern.MatchString(logicalPath) {
		return logicalPath
	}
	digest := sha256.Sum256([]byte(logicalPath))
	return "file-" + hex.EncodeToString(digest[:])
}
