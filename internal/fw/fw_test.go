package fw

import (
	"os"
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
	tmpfile, err := os.CreateTemp("", "caps")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString("playlist library\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	tmpfile.Close()

	caps := ReadCaps(tmpfile.Name())
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
	tmpfile, err := os.CreateTemp("", "library")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())
	tmpfile.Close()

	err = RequestLibrary(tmpfile.Name(), 7, []string{"/mnt/mmc/_explaylist_data/Navidrome - Mix.m3u"})
	if err != nil {
		t.Fatalf("RequestLibrary: %v", err)
	}

	data, err := os.ReadFile(tmpfile.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	expected := "7\nscan\nimport /mnt/mmc/_explaylist_data/Navidrome - Mix.m3u\n"
	if string(data) != expected {
		t.Errorf("RequestLibrary bytes: got %q, want %q", string(data), expected)
	}
}

func TestRequestLibraryBadImportPath(t *testing.T) {
	// RequestLibrary should reject paths not in /mnt/mmc/_explaylist_data/ or not ending in .m3u
	tmpfile, err := os.CreateTemp("", "library")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())
	tmpfile.Close()

	// Path not in /mnt/mmc/_explaylist_data/
	err = RequestLibrary(tmpfile.Name(), 7, []string{"/home/user/playlist.m3u"})
	if err == nil {
		t.Error("RequestLibrary with bad path: got nil, want error")
	}

	// Path in right dir but doesn't end in .m3u
	err = RequestLibrary(tmpfile.Name(), 7, []string{"/mnt/mmc/_explaylist_data/playlist.txt"})
	if err == nil {
		t.Error("RequestLibrary with bad extension: got nil, want error")
	}
}

func TestLibraryDoneParsesError(t *testing.T) {
	// "7 error Card busy" should give (true, false, "Card busy")
	tmpfile, err := os.CreateTemp("", "done")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString("7 error Card busy"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	tmpfile.Close()

	done, ok, msg := LibraryDone(tmpfile.Name(), 7)
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
	tmpfile, err := os.CreateTemp("", "done")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString("5 ok"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	tmpfile.Close()

	done, _, _ := LibraryDone(tmpfile.Name(), 7)
	if done {
		t.Errorf("done with old seq: got true, want false")
	}
}

func TestLibraryDoneSuccess(t *testing.T) {
	// "7 ok" should give (true, true, "")
	tmpfile, err := os.CreateTemp("", "done")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.WriteString("7 ok"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	tmpfile.Close()

	done, ok, msg := LibraryDone(tmpfile.Name(), 7)
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
