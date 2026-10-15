package reel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"testing"
	"unicode/utf16"
)

// Independent pins prevent an edited manifest from blessing edited assets.
var libraryAssetPins = map[string]string{
	"player.js":          "4d63cfb88859f158d1eef5d8a5550861abcb7ec195fb5d684f35f6446a29fe11",
	"player.css":         "f619fe17597043564f03b2c6918b3daf890ee8b912fb408542fba11afade4fdb",
	"DejaVuSansMono.ttf": "9d9bfebceb1c3f6f4ad383ded568a6926086208f43f7f92f92f7e93a1383fa38",
	// Generated from the pinned DejaVuSansMono.ttf by scripts/box-font.py.
	"AgentReelBox.ttf":   "71ac0c5f25723bd4fad00a57737dc8e964292de50c2b4b320e5bd388a440669c",
	"LICENSE-player.txt": "63422284ed65be54ab38d8a94e7518d27e222fbb26c9ae4cd984c63926e91c12",
	"LICENSE-font.txt":   "6f57fb3cc4e1d62de5ce0ee6444e6ca45eb8a35aaf0bf70a694f9cc133c5881d",
}

func TestLibraryAssetPinsAndLicenses(t *testing.T) {
	var manifest struct {
		Version int               `json:"version"`
		Player  map[string]string `json:"player"`
		Font    map[string]string `json:"font"`
		BoxFont map[string]string `json:"boxFont"`
		Files   map[string]struct {
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	raw, err := libraryWeb.ReadFile("web/vendor/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatal("trailing asset manifest data")
	}
	expectedPlayer := map[string]string{"name": "asciinema-player", "version": "3.17.0", "license": "Apache-2.0", "source": "https://registry.npmjs.org/asciinema-player/-/asciinema-player-3.17.0.tgz", "integrity": "sha512-JbjNJmA2TLIeYNaOEja+kVSzXadKoqpIzVVmfBGNj2DmdtE/vExBCnkE8NYEcpaQcDvUYg8Ltt0urT80frACmw==", "gitHead": "8ba9c54008ed1f25d2914abe31ea118dbf7da2e4", "packageSHA256": "93a5932f39f24b5a4309fb33ab4dc33fd63e2960c0c623501d942196fe80528d"}
	expectedFont := map[string]string{"name": "DejaVu Sans Mono", "version": "2.37", "source": "https://dejavu-fonts.github.io/", "license": "Bitstream Vera / DejaVu public-domain changes; extracted verbatim from nameID13"}
	expectedBoxFont := map[string]string{"name": "AgentReel Box", "derivedFrom": "DejaVu Sans Mono 2.37", "generator": "scripts/box-font.py (fonttools)", "unicodeRange": "U+2500-257F", "license": "Bitstream Vera / DejaVu license (LICENSE-font.txt, kept in nameID0/13/14); modified (box-drawing verticals stretched to a 1.3333em line box) and renamed as that license requires"}
	if manifest.Version != 1 || !reflect.DeepEqual(manifest.Player, expectedPlayer) || !reflect.DeepEqual(manifest.Font, expectedFont) || !reflect.DeepEqual(manifest.BoxFont, expectedBoxFont) || len(manifest.Files) != len(libraryAssetPins) {
		t.Fatal("asset manifest provenance changed")
	}
	names := []string{"manifest.json"}
	for name, pin := range libraryAssetPins {
		names = append(names, name)
		raw, err := libraryWeb.ReadFile("web/vendor/" + name)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != pin {
			t.Fatalf("independent pin mismatch: %s", name)
		}
		if entry, ok := manifest.Files[name]; !ok || entry.Bytes != len(raw) || entry.SHA256 != pin {
			t.Fatalf("manifest mismatch: %s", name)
		}
	}
	entries, err := libraryWeb.ReadDir("web/vendor")
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatal("vendor directory not allowed")
		}
		actual = append(actual, entry.Name())
	}
	sort.Strings(names)
	sort.Strings(actual)
	if !reflect.DeepEqual(names, actual) {
		t.Fatalf("unexpected vendor inventory: %v", actual)
	}
	font, _ := libraryWeb.ReadFile("web/vendor/DejaVuSansMono.ttf")
	license, err := libraryFontLicense(font)
	if err != nil {
		t.Fatal(err)
	}
	shipped, _ := libraryWeb.ReadFile("web/vendor/LICENSE-font.txt")
	if !bytes.Equal(license, shipped) {
		t.Fatal("font license differs from embedded TrueType nameID13")
	}
	// The derived box-drawing font carries the same license, verbatim, and
	// is renamed as that license requires for a modified font.
	box, _ := libraryWeb.ReadFile("web/vendor/AgentReelBox.ttf")
	if boxLicense, err := libraryFontLicense(box); err != nil || !bytes.Equal(boxLicense, shipped) {
		t.Fatal("box font license differs from the DejaVu license", err)
	}
	for _, name := range []string{"DejaVu", "Bitstream", "Vera"} {
		if libraryFontNamed(box, name) {
			t.Fatalf("modified font still named %q", name)
		}
	}
}

// Read the Windows UTF-16BE nameID13 record, preserving the exact upstream
// license text rather than comparing only a filename or a manifest assertion.
func libraryFontLicense(data []byte) ([]byte, error) {
	part := func(data []byte, start, length uint64) ([]byte, error) {
		if start > uint64(len(data)) || length > uint64(len(data))-start {
			return nil, fmt.Errorf("font bounds")
		}
		return data[start : start+length], nil
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("font header")
	}
	for i := 0; i < int(binary.BigEndian.Uint16(data[4:6])); i++ {
		record, err := part(data, uint64(12+i*16), 16)
		if err != nil {
			return nil, err
		}
		if string(record[:4]) != "name" {
			continue
		}
		table, err := part(data, uint64(binary.BigEndian.Uint32(record[8:12])), uint64(binary.BigEndian.Uint32(record[12:16])))
		if err != nil || len(table) < 6 {
			return nil, fmt.Errorf("font name table")
		}
		for j := 0; j < int(binary.BigEndian.Uint16(table[2:4])); j++ {
			record, err := part(table, uint64(6+j*12), 12)
			if err != nil {
				return nil, err
			}
			if binary.BigEndian.Uint16(record[:2]) != 3 || binary.BigEndian.Uint16(record[6:8]) != 13 {
				continue
			}
			start := uint64(binary.BigEndian.Uint16(table[4:6])) + uint64(binary.BigEndian.Uint16(record[10:12]))
			raw, err := part(table, start, uint64(binary.BigEndian.Uint16(record[8:10])))
			if err != nil || len(raw)%2 != 0 {
				return nil, fmt.Errorf("font license encoding")
			}
			units := make([]uint16, len(raw)/2)
			for n := range units {
				units[n] = binary.BigEndian.Uint16(raw[2*n : 2*n+2])
			}
			for n := 0; n < len(units); n++ {
				if units[n] >= 0xD800 && units[n] <= 0xDBFF {
					n++
					if n >= len(units) || units[n] < 0xDC00 || units[n] > 0xDFFF {
						return nil, fmt.Errorf("invalid font surrogate")
					}
				} else if units[n] >= 0xDC00 && units[n] <= 0xDFFF {
					return nil, fmt.Errorf("invalid font surrogate")
				}
			}
			return []byte(string(utf16.Decode(units)) + "\n"), nil
		}
	}
	return nil, fmt.Errorf("font license unavailable")
}

// libraryFontNamed reports whether a Windows name record naming the font
// (family, subfamily, unique, full, PostScript, typographic names) contains
// text. Copyright, description and license records are not names.
func libraryFontNamed(data []byte, text string) bool {
	if len(data) < 12 {
		return false
	}
	for i := 0; i < int(binary.BigEndian.Uint16(data[4:6])) && 12+i*16+16 <= len(data); i++ {
		record := data[12+i*16 : 12+i*16+16]
		if string(record[:4]) != "name" {
			continue
		}
		start, length := int(binary.BigEndian.Uint32(record[8:12])), int(binary.BigEndian.Uint32(record[12:16]))
		if start+length > len(data) || length < 6 {
			return false
		}
		table := data[start : start+length]
		for j := 0; j < int(binary.BigEndian.Uint16(table[2:4])) && 6+j*12+12 <= len(table); j++ {
			r := table[6+j*12 : 6+j*12+12]
			switch binary.BigEndian.Uint16(r[6:8]) {
			case 1, 2, 3, 4, 6, 16, 17, 21, 22:
			default:
				continue
			}
			offset := int(binary.BigEndian.Uint16(table[4:6])) + int(binary.BigEndian.Uint16(r[10:12]))
			raw := table[min(offset, len(table)):min(offset+int(binary.BigEndian.Uint16(r[8:10])), len(table))]
			if binary.BigEndian.Uint16(r[:2]) == 3 {
				units := make([]uint16, len(raw)/2)
				for n := range units {
					units[n] = binary.BigEndian.Uint16(raw[2*n:])
				}
				raw = []byte(string(utf16.Decode(units)))
			}
			if bytes.Contains(raw, []byte(text)) {
				return true
			}
		}
	}
	return false
}

func TestLibraryAssetFontLicenseBounds(t *testing.T) {
	for _, data := range [][]byte{nil, make([]byte, 12), {0, 0, 0, 0, 255, 255, 0, 0, 0, 0, 0, 0}} {
		if _, err := libraryFontLicense(data); err == nil {
			t.Fatal("invalid font accepted")
		}
	}
}
