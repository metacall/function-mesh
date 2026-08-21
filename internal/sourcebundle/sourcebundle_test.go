package sourcebundle

import (
	"encoding/json"
	"testing"
)

func TestEncodeAndVolumeItemsPreserveNestedPaths(t *testing.T) {
	encoded, err := Encode(map[string]string{
		"metacall.json":        `{"language_id":"py"}`,
		"services/api/main.py": "def main():\n    return 1\n",
	})
	if err != nil {
		t.Fatalf("encode bundle: %v", err)
	}
	if encoded["metacall.json"] == "" {
		t.Fatalf("safe flat key was not preserved")
	}
	if _, exists := encoded["services/api/main.py"]; exists {
		t.Fatalf("nested path cannot be used directly as a ConfigMap key")
	}

	items, indexed, err := VolumeItems(encoded)
	if err != nil {
		t.Fatalf("decode volume items: %v", err)
	}
	if !indexed || len(items) != 2 {
		t.Fatalf("unexpected volume items: indexed=%v items=%#v", indexed, items)
	}
	if items[0].Path != "metacall.json" || items[1].Path != "services/api/main.py" {
		t.Fatalf("nested paths were not preserved: %#v", items)
	}
}

func TestVolumeItemsRejectsUnsafeOrIncompleteIndex(t *testing.T) {
	index, _ := json.Marshal(map[string]string{"file-a": "../secret"})
	_, indexed, err := VolumeItems(map[string]string{IndexKey: string(index), "file-a": "value"})
	if !indexed || err == nil {
		t.Fatalf("expected unsafe index error, got indexed=%v err=%v", indexed, err)
	}

	index, _ = json.Marshal(map[string]string{"missing": "main.py"})
	if _, _, err := VolumeItems(map[string]string{IndexKey: string(index)}); err == nil {
		t.Fatalf("expected missing key error")
	}

	index, _ = json.Marshal(map[string]string{"file-a": "main.py", "file-b": "main.py"})
	if _, _, err := VolumeItems(map[string]string{IndexKey: string(index), "file-a": "a", "file-b": "b"}); err == nil {
		t.Fatalf("expected duplicate path error")
	}
}

func TestEncodeRejectsUnsafePath(t *testing.T) {
	if _, err := Encode(map[string]string{"../secret": "value"}); err == nil {
		t.Fatalf("expected unsafe path error")
	}
}
