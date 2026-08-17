package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ubilolxon/qrmenu/internal/config"
)

// Most of this package is a thin wrapper over minio-go and needs a live bucket
// to mean anything. What is tested here is the part that is ours: URL building,
// the policy document, and the guard that stops a prefix delete becoming a
// wider delete than intended.

func testClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(config.Storage{
		Endpoint:  "localhost:9000",
		AccessKey: "k",
		SecretKey: "s",
		Bucket:    "qrmenu",
		Region:    "us-east-1",
		PublicURL: "https://cdn.example.test/qrmenu/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRequiresAnEndpoint(t *testing.T) {
	if _, err := New(config.Storage{Bucket: "qrmenu"}); err == nil {
		t.Error("New accepted an empty endpoint")
	}
}

// A trailing slash on the configured base must not produce a double slash: some
// CDNs treat //a and /a as different objects and cache them separately.
func TestPublicURLNormalisesTheBase(t *testing.T) {
	c := testClient(t)
	got := c.PublicURL("restaurants/r/images/i/thumb.jpg")
	want := "https://cdn.example.test/qrmenu/restaurants/r/images/i/thumb.jpg"
	if got != want {
		t.Errorf("PublicURL() = %q, want %q", got, want)
	}
}

// The trailing slash is the whole safety property: "images/abc" without it
// would also match "images/abcdef", so deleting one image could take another's
// objects with it.
func TestRemovePrefixRefusesUnboundedPrefixes(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	for _, prefix := range []string{"", "restaurants", "restaurants/r/images/i"} {
		if err := c.RemovePrefix(ctx, prefix); err == nil {
			t.Errorf("RemovePrefix(%q) was accepted; it must end in a slash", prefix)
		} else if !strings.Contains(err.Error(), "non-directory prefix") {
			t.Errorf("RemovePrefix(%q) failed for the wrong reason: %v", prefix, err)
		}
	}
}

func TestReadOnlyPolicy(t *testing.T) {
	var doc struct {
		Version   string
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(readOnlyPolicy("qrmenu")), &doc); err != nil {
		t.Fatalf("the policy is not valid JSON: %v", err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("got %d statements, want 1", len(doc.Statement))
	}

	st := doc.Statement[0]
	if st.Effect != "Allow" {
		t.Errorf("Effect = %q", st.Effect)
	}
	// Read only. s3:ListBucket would let anyone enumerate every image on the
	// platform, and any write action would be a public upload endpoint.
	if len(st.Action) != 1 || st.Action[0] != "s3:GetObject" {
		t.Errorf("Action = %v, want exactly [s3:GetObject]", st.Action)
	}
	if len(st.Resource) != 1 || st.Resource[0] != "arn:aws:s3:::qrmenu/*" {
		t.Errorf("Resource = %v, want the bucket's objects only", st.Resource)
	}
}
