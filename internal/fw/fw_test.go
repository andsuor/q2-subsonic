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

	err := RequestLibrary(libfile, 7, true, []string{"/mnt/mmc/_explaylist_data/Mix.m3u"}, nil)
	if err != nil {
		t.Fatalf("RequestLibrary: %v", err)
	}

	data, err := os.ReadFile(libfile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	expected := "7\nscan\nimport /mnt/mmc/_explaylist_data/Mix.m3u\n"
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

		// Valid: an ellipsis in the name; the firmware refuses only a real ".." segment
		{"ellipsis_name", "/mnt/mmc/_explaylist_data/Wait... What.m3u", false},

		// Invalid: the name is ".."
		{"dotdot_name", "/mnt/mmc/_explaylist_data/...m3u", true},

		// Invalid: wrong directory
		{"wrong_dir", "/home/user/playlist.m3u", true},

		// Invalid: no extension
		{"no_ext", "/mnt/mmc/_explaylist_data/playlist.txt", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpdir := t.TempDir()
			libfile := filepath.Join(tmpdir, "library")

			err := RequestLibrary(libfile, 7, true, []string{tt.path}, nil)

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

	done, ok, msg, _ := LibraryDone(donefile, 7)
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

	done, _, _, _ := LibraryDone(donefile, 7)
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

	done, ok, msg, _ := LibraryDone(donefile, 7)
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

	done, ok, msg, _ := LibraryDone(donefile, 7)
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

// R35: "import" for playlists the app owns (a same-named one is replaced), "add" for the rest
// (skipped when the Q2 has one of that name); a follow-up request has no scan.
func TestRequestLibraryAddLines(t *testing.T) {
	f := filepath.Join(t.TempDir(), "library")
	a, b := "/mnt/mmc/_explaylist_data/Mix.m3u", "/mnt/mmc/_explaylist_data/Rock.m3u"
	if err := RequestLibrary(f, 7, true, []string{a}, []string{b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "7\nscan\nimport "+a+"\nadd "+b+"\n" {
		t.Fatalf("%q", got)
	}
	if seq, m3us := ReadRequest(f); seq != 7 || len(m3us) != 2 || m3us[0] != a || m3us[1] != b {
		t.Fatal(seq, m3us)
	}
	if err := RequestLibrary(f, 8, false, nil, []string{b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(f); string(got) != "8\nadd "+b+"\n" {
		t.Fatalf("%q", got)
	}
	if err := RequestLibrary(f, 9, false, nil, []string{"/mnt/mmc/_explaylist_data/sub/Mix.m3u"}); err == nil {
		t.Fatal("bad add path taken")
	}
	if seq, _ := ReadRequest(filepath.Join(t.TempDir(), "none")); seq != 0 {
		t.Fatal(seq)
	}
}

// After line 1, "exists <path>" lines name the adds the firmware skipped (R35).
func TestLibraryDoneExists(t *testing.T) {
	f := filepath.Join(t.TempDir(), "done")
	os.WriteFile(f, []byte("7 ok\nexists /mnt/mmc/_explaylist_data/A.m3u\nexists /mnt/mmc/_explaylist_data/B.m3u\n"), 0o644)
	done, ok, msg, exists := LibraryDone(f, 7)
	if !done || !ok || msg != "" || len(exists) != 2 || exists[1] != "/mnt/mmc/_explaylist_data/B.m3u" {
		t.Fatal(done, ok, msg, exists)
	}
	os.WriteFile(f, []byte("7 error Couldn't import A\nexists /mnt/mmc/_explaylist_data/B.m3u\n"), 0o644)
	if done, ok, msg, _ := LibraryDone(f, 7); !done || ok || msg != "Couldn't import A" {
		t.Fatal(done, ok, msg)
	}
}
