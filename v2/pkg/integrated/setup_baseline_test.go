package integrated

import "testing"

func TestVisualBaselineFilesSelectsOnlyReviewableSnapshots(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".visual-hive/snapshots/home.png", "png")
	writeFixture(t, root, ".visual-hive/snapshots/linux/jobs.PNG", "png")
	writeFixture(t, root, ".visual-hive/snapshots/readme.txt", "not a baseline")
	writeFixture(t, root, ".visual-hive/artifacts/actual.png", "not a baseline")
	files, err := visualBaselineFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != ".visual-hive/snapshots/home.png" || files[1] != ".visual-hive/snapshots/linux/jobs.PNG" {
		t.Fatalf("baseline review candidates = %v", files)
	}
}
