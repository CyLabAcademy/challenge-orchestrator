package cmgr

import "testing"

func TestImageDigestMatches(t *testing.T) {
	digest := "sha256:6aea1af2ab09dfa5f20b7c93d2325ee29a8104e2b8ab853d5b60dbc23ee1a2af"
	other := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	tests := []struct {
		name        string
		repoDigests []string
		digest      string
		want        bool
	}{
		{
			name:        "registry-qualified match",
			repoDigests: []string{"10.12.34.121:5000/picoctf/north-south@" + digest},
			digest:      digest,
			want:        true,
		},
		{
			name:        "match among multiple entries",
			repoDigests: []string{"example.com/foo@" + other, "10.12.34.121:5000/picoctf/north-south@" + digest},
			digest:      digest,
			want:        true,
		},
		{
			name:        "different digest",
			repoDigests: []string{"10.12.34.121:5000/picoctf/north-south@" + other},
			digest:      digest,
			want:        false,
		},
		{
			name:        "no repo digests (locally built, never pulled/pushed)",
			repoDigests: []string{},
			digest:      digest,
			want:        false,
		},
		{
			name:        "empty stored digest never matches",
			repoDigests: []string{"10.12.34.121:5000/picoctf/north-south@" + digest},
			digest:      "",
			want:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageDigestMatches(tc.repoDigests, tc.digest); got != tc.want {
				t.Errorf("imageDigestMatches(%v, %q) = %v, want %v", tc.repoDigests, tc.digest, got, tc.want)
			}
		})
	}
}

func TestExtractPushDigest(t *testing.T) {
	digest := "sha256:6aea1af2ab09dfa5f20b7c93d2325ee29a8104e2b8ab853d5b60dbc23ee1a2af"

	tests := []struct {
		name     string
		messages string
		want     string
	}{
		{
			name: "typical push stream",
			messages: `{"status":"The push refers to repository [10.12.34.121:5000/picoctf/north-south]"}
{"status":"Pushed","progressDetail":{},"id":"5f70bf18a086"}
{"status":"2-challenge: digest: ` + digest + ` size: 1786"}
{"progressDetail":{},"aux":{"Tag":"2-challenge","Digest":"` + digest + `","Size":1786}}
`,
			want: digest,
		},
		{
			name: "containerd image store stream (status line, no aux)",
			messages: `{"status":"Pushed","progressDetail":{},"id":"5f70bf18a086"}
{"status":"8-challenge: digest: ` + digest + ` size: 1786"}
`,
			want: digest,
		},
		{
			name:     "no aux message or digest status line",
			messages: `{"status":"Pushed","progressDetail":{},"id":"5f70bf18a086"}`,
			want:     "",
		},
		{
			name: "multiple aux messages takes the last",
			messages: `{"aux":{"Tag":"a","Digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","Size":1}}
{"aux":{"Tag":"b","Digest":"` + digest + `","Size":2}}
`,
			want: digest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractPushDigest([]byte(tc.messages)); got != tc.want {
				t.Errorf("extractPushDigest(...) = %q, want %q", got, tc.want)
			}
		})
	}
}
