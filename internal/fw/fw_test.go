package fw

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapsMissing(t *testing.T) {
	// Missing file should give all false
	caps := ReadCaps("/nonexistent/path/to/caps")
	if caps.Playlist || caps.Progress || caps.Library {
		t.Errorf("ReadCaps missing file: got %+v, want all false", caps)
	}
}

func TestCapsWords(t *testing.T) {
	// "playlist library\n" should give Playlist and Library, but not Progress
	tmpdir := t.TempDir()
	capsfile := filepath.Join(tmpdir, "caps")

	if err := os.WriteFile(capsfile, []byte("playlist library\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	caps := ReadCaps(capsfile)
	if !caps.Playlist {
		t.Errorf("Playlist: got false, want true")
	}
	if caps.Progress {
		t.Errorf("Progress: got true, want false")
	}
	if !caps.Library {
		t.Errorf("Library: got false, want true")
	}
}

func TestRequestLibraryFormat(t *testing.T) {
	// RequestLibrary should write exact bytes
	tmpdir := t.TempDir()
	libfile := filepath.Join(tmpdir, "library")

	err := RequestLibrary(libfile, 7, []string{"/mnt/mmc/_explaylist_data/Navidrome - Mix.m3u"})
	if err != nil {
		t.Fatalf("RequestLibrary: %v", err)
	}

	data, err := os.ReadFile(libfile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	expected := "7\nscan\nimport /mnt/mmc/_explaylist_data/Navidrome - Mix.m3u\n"
	if string(data) != expected {
		t.Errorf("RequestLibrary bytes: got %q, want %q", string(data), expected)
	}
}

func TestRequestLibraryBadPaths(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		// Valid path
		{"valid", "/mnt/mmc/_explaylist_data/Playlist.m3u", false},

		// Invalid: nested path
		{"nested", "/mnt/mmc/_explaylist_data/subdir/Playlist.m3u", true},

		// Invalid: .. in path
		{"parent_ref", "/mnt/mmc/_explaylist_data/../Playlist.m3u", true},

		// Invalid: path with newline
		{"newline", "/mnt/mmc/_explaylist_data/Play\nlist.m3u", true},

		// Invalid: path with carriage return
		{"carriage_return", "/mnt/mmc/_explaylist_data/Play\rlist.m3u", true},

		// Invalid: bare .m3u (no base name)
		{"bare_m3u", "/mnt/mmc/_explaylist_data/.m3u", true},

		// Invalid: wrong directory
		{"wrong_dir", "/home/user/playlist.m3u", true},

		// Invalid: no extension
		{"no_ext", "/mnt/mmc/_explaylist_data/playlist.txt", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpdir := t.TempDir()
			libfile := filepath.Join(tmpdir, "library")

			err := RequestLibrary(libfile, 7, []string{tt.path})

			if (err != nil) != tt.wantErr {
				t.Errorf("RequestLibrary error: got %v, want error=%v", err, tt.wantErr)
			}

			// After rejection, file should not exist
			if tt.wantErr {
				if _, err := os.Stat(libfile); !os.IsNotExist(err) {
					t.Errorf("RequestLibrary should not write file on error, but file exists")
				}
			}
		})
	}
}

func TestLibraryDoneParsesError(t *testing.T) {
	// "7 error Card busy" should give (true, false, "Card busy")
	tmpdir := t.TempDir()
	donefile := filepath.Join(tmpdir, "done")

	if err := os.WriteFile(donefile, []byte("7 error Card busy"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	done, ok, msg := LibraryDone(donefile, 7)
	if !done {
		t.Errorf("done: got false, want true")
	}
	if ok {
		t.Errorf("ok: got true, want false")
	}
	if msg != "Card busy" {
		t.Errorf("msg: got %q, want %q", msg, "Card busy")
	}
}

func TestLibraryDoneOldSeq(t *testing.T) {
	// An old seq means not done for this request
	tmpdir := t.TempDir()
	donefile := filepath.Join(tmpdir, "done")

	if err := os.WriteFile(donefile, []byte("5 ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	done, _, _ := LibraryDone(donefile, 7)
	if done {
		t.Errorf("done with old seq: got true, want false")
	}
}

func TestLibraryDoneSuccess(t *testing.T) {
	// "7 ok" should give (true, true, "")
	tmpdir := t.TempDir()
	donefile := filepath.Join(tmpdir, "done")

	if err := os.WriteFile(donefile, []byte("7 ok"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	done, ok, msg := LibraryDone(donefile, 7)
	if !done {
		t.Errorf("done: got false, want true")
	}
	if !ok {
		t.Errorf("ok: got false, want true")
	}
	if msg != "" {
		t.Errorf("msg: got %q, want %q", msg, "")
	}
}

func TestLibraryDoneMissing(t *testing.T) {
	// Missing file should give not done
	tmpdir := t.TempDir()
	donefile := filepath.Join(tmpdir, "nonexistent")

	done, ok, msg := LibraryDone(donefile, 7)
	if done {
		t.Errorf("done: got true, want false")
	}
	if ok {
		t.Errorf("ok: got true, want false")
	}
	if msg != "" {
		t.Errorf("msg: got %q, want %q", msg, "")
	}
}
