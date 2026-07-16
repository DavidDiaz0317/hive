package main

import (
	"strings"
	"testing"
)

func TestHostedRestoreReleaseRequiresCompleteDistinctIdentity(t *testing.T) {
	t.Parallel()
	current := installedReleaseIdentity{
		Version:          "v0.4.1-integrated.19",
		HiveCommit:       strings.Repeat("a", 40),
		VisualHiveCommit: strings.Repeat("b", 40),
		ManifestSHA256:   strings.Repeat("c", 64),
	}

	restore, manifest, err := hostedRestoreRelease("", "", "", "", current)
	if err != nil || restore != nil || manifest != nil {
		t.Fatalf("empty predecessor = (%+v, %+v, %v), want nil identity", restore, manifest, err)
	}

	version := "v0.4.1-integrated.18"
	hiveCommit := strings.Repeat("d", 40)
	visualCommit := strings.Repeat("e", 40)
	manifestDigest := strings.Repeat("f", 64)
	restore, manifest, err = hostedRestoreRelease(version, hiveCommit, visualCommit, manifestDigest, current)
	if err != nil {
		t.Fatalf("complete predecessor: %v", err)
	}
	if restore == nil || restore.Version != version || restore.HiveCommit != hiveCommit || restore.VisualHiveCommit != visualCommit {
		t.Fatalf("restore identity = %+v", restore)
	}
	if manifest == nil || manifest.Version != version || manifest.HiveCommit != hiveCommit || manifest.VisualHiveCommit != visualCommit || manifest.DistributionManifestSHA256 != manifestDigest {
		t.Fatalf("manifest identity = %+v", manifest)
	}

	tests := []struct {
		name           string
		version        string
		hiveCommit     string
		visualCommit   string
		manifestDigest string
	}{
		{name: "partial", version: version},
		{name: "invalid version", version: "integrated.18", hiveCommit: hiveCommit, visualCommit: visualCommit, manifestDigest: manifestDigest},
		{name: "invalid hive commit", version: version, hiveCommit: "not-a-commit", visualCommit: visualCommit, manifestDigest: manifestDigest},
		{name: "same version", version: current.Version, hiveCommit: hiveCommit, visualCommit: visualCommit, manifestDigest: manifestDigest},
		{name: "same hive commit", version: version, hiveCommit: current.HiveCommit, visualCommit: visualCommit, manifestDigest: manifestDigest},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := hostedRestoreRelease(test.version, test.hiveCommit, test.visualCommit, test.manifestDigest, current); err == nil {
				t.Fatal("invalid predecessor identity was accepted")
			}
		})
	}
}
