package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootDiskSizeMB(t *testing.T) {
	cases := []struct {
		name                      string
		used, requested, min, max int
		want                      int
		wantErr                   bool
	}{
		{"small image uses minimum", 50, 0, 512, 65536, 512, false},
		{"large image grows with contents", 2000, 0, 512, 65536, 2000 + 600 + 1 + rootDiskHeadroomMB, false},
		{"automatic size capped at max", 1000, 0, 512, 1400, 1400, false},
		{"image larger than max fails", 2000, 0, 512, 2000, 0, true},
		{"explicit request honoured", 100, 4096, 512, 65536, 4096, false},
		{"explicit request below minimum is allowed if it fits", 100, 200, 512, 65536, 200, false},
		{"explicit request too small for image", 1000, 1100, 512, 65536, 0, true},
		{"explicit request above max", 100, 70000, 512, 65536, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rootDiskSizeMB(tc.used, tc.requested, tc.min, tc.max)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got size %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("rootDiskSizeMB() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDirUsageBytesRoundsToBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "one-byte"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two-blocks"), make([]byte, ext4BlockSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := dirUsageBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 4 entries (root, sub, two files) at one block each, plus 1 + 2 data blocks.
	want := int64(4*ext4BlockSize + 3*ext4BlockSize)
	if got != want {
		t.Fatalf("dirUsageBytes() = %d, want %d", got, want)
	}
}
