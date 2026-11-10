package dataset

// These tests cover reading a dataset's objects from a list the operator supplies.
//
// The list is the way a dataset larger than one wildcard's expansion limit is copied at
// all, and it is assembled by hand against a store this code cannot see. So what matters
// is not that a well-formed list parses -- it is that a key belonging to a neighbouring
// folder, or to another bucket, is refused rather than quietly copied into a cache bucket
// whose copy record does not describe it.

import (
	"strings"
	"testing"
)

const listDataset = "s3://source-bucket/datasets/cohort-a/"

func TestParseObjectListAcceptsTheFormsAListingProduces(t *testing.T) {
	list := `
# taken from the source store on the 3rd
datasets/cohort-a/reads/sample.bam
s3://source-bucket/datasets/cohort-a/reads/sample.bam.bai

datasets/cohort-a/notes.txt
`
	objects, err := ParseObjectList(listDataset, strings.NewReader(list))
	if err != nil {
		t.Fatalf("parsing the list: %v", err)
	}

	// Every key comes back relative to the dataset's prefix, which is the form a copy
	// instruction carries, whichever way the line was written.
	want := []string{"reads/sample.bam", "reads/sample.bam.bai", "notes.txt"}
	if len(objects) != len(want) {
		t.Fatalf("expected %d objects, got %d: %+v", len(want), len(objects), objects)
	}
	for i, key := range want {
		if objects[i].Key != key {
			t.Errorf("object %d = %q, want %q", i, objects[i].Key, key)
		}
	}
}

// TestParseObjectListRefusesKeysOutsideTheDataset is the check the list exists for. A
// dataset is released as a unit and its release names one folder; an object from anywhere
// else would be copied into a bucket whose policy grants access to a dataset that does not
// contain it.
func TestParseObjectListRefusesKeysOutsideTheDataset(t *testing.T) {
	cases := map[string]string{
		"a neighbouring folder":    "datasets/cohort-b/reads/sample.bam",
		"another bucket":           "s3://other-bucket/datasets/cohort-a/reads/sample.bam",
		"a key above the prefix":   "datasets/sample.bam",
		"the dataset's own prefix": "datasets/cohort-a/",
		"a scheme that is not s3":  "gs://source-bucket/datasets/cohort-a/reads/sample.bam",
		"a line naming no object":  "s3://source-bucket/",
	}

	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseObjectList(listDataset, strings.NewReader(line+"\n")); err == nil {
				t.Errorf("expected %q to be refused", line)
			}
		})
	}
}

// TestParseObjectListReportsEveryBadLineAtOnce matters because a list is assembled in
// bulk, so its mistakes arrive in bulk. Fixing them one run at a time is no way to spend
// an hour.
func TestParseObjectListReportsEveryBadLineAtOnce(t *testing.T) {
	list := strings.Join([]string{
		"datasets/cohort-a/good.bam",
		"datasets/cohort-b/wrong-folder.bam",
		"s3://other-bucket/datasets/cohort-a/wrong-bucket.bam",
	}, "\n")

	_, err := ParseObjectList(listDataset, strings.NewReader(list))
	if err == nil {
		t.Fatal("expected a list with bad lines to be refused")
	}
	for _, want := range []string{"line 2", "line 3", "cohort-b", "other-bucket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the report to name %q, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "line 1") {
		t.Errorf("the good line was reported as a problem: %v", err)
	}
}

// TestParseObjectListRefusesAnEmptyList guards the outcome that would otherwise be silent:
// a dataset copied from nothing, recorded as staged, and granted having delivered nothing.
func TestParseObjectListRefusesAnEmptyList(t *testing.T) {
	for name, list := range map[string]string{
		"nothing at all":   "",
		"only blank lines": "\n\n   \n",
		"only comments":    "# nothing here yet\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseObjectList(listDataset, strings.NewReader(list))
			if err == nil {
				t.Fatal("expected an empty list to be refused")
			}
			if !strings.Contains(err.Error(), "names no objects") {
				t.Errorf("expected the refusal to say the list is empty, got: %v", err)
			}
		})
	}
}

// TestParseObjectListFeedsInstructionsThatCarryNoWildcard ties the list to what it is for:
// the instructions it produces are headed one by one, so none of them is subject to the
// expansion limit that made the list necessary.
func TestParseObjectListFeedsInstructionsThatCarryNoWildcard(t *testing.T) {
	objects, err := ParseObjectList(submitDatasetURI, strings.NewReader(
		submitDatasetPrefix+"reads/sample.bam\n"+submitDatasetPrefix+"reads/sample.bam.bai\n"))
	if err != nil {
		t.Fatalf("parsing the list: %v", err)
	}

	req := copyOf(submitDatasetURI)
	req.Objects = objects
	plan, err := PlanCopy(submitConfig(), req)
	if err != nil {
		t.Fatalf("planning the copy: %v", err)
	}

	if len(plan.Instructions) != len(objects) {
		t.Fatalf("expected one instruction per object, got %d for %d objects",
			len(plan.Instructions), len(objects))
	}
	for _, instruction := range plan.Instructions {
		if strings.HasSuffix(instruction.SourceKey, "*") {
			t.Errorf("a named object produced a wildcard instruction: %q", instruction.SourceKey)
		}
		if instruction.SourceRootFolderKey == "" {
			t.Errorf("%s states no root folder, so the copy would flatten the dataset",
				instruction.SourceKey)
		}
	}
}

// TestCanonicalDatasetURIReducesTheFormsAnOperatorWrites covers the identity of a dataset.
//
// The URI is compared by exact string equality in several places at once -- the key its
// copies and grants are found under, the input its bucket name is derived from, the key a
// pinned bucket is looked up by -- and nothing checks that those agree. So a difference nobody
// would think twice about, a missing scheme or a missing trailing slash, silently names a
// second dataset that has never been staged and holds nothing.
func TestCanonicalDatasetURIReducesTheFormsAnOperatorWrites(t *testing.T) {
	const canonical = "s3://source-bucket/datasets/cohort-a/"

	for _, written := range []string{
		canonical,
		"s3://source-bucket/datasets/cohort-a",
		"source-bucket/datasets/cohort-a/",
		"source-bucket/datasets/cohort-a",
		"  s3://source-bucket/datasets/cohort-a/  ",
	} {
		got, err := CanonicalDatasetURI(written)
		if err != nil {
			t.Errorf("CanonicalDatasetURI(%q): %v", written, err)
			continue
		}
		if got != canonical {
			t.Errorf("CanonicalDatasetURI(%q) = %q, want %q", written, got, canonical)
		}
	}

	// A bucket with no prefix is the whole bucket, and still has one spelling.
	whole, err := CanonicalDatasetURI("source-bucket")
	if err != nil {
		t.Fatalf("CanonicalDatasetURI of a bare bucket: %v", err)
	}
	if whole != "s3://source-bucket/" {
		t.Errorf("a bare bucket canonicalised to %q, want %q", whole, "s3://source-bucket/")
	}

	// Reducing twice changes nothing, which is what lets it be applied wherever a URI
	// arrives without having to know whether it has been applied already.
	twice, err := CanonicalDatasetURI(canonical)
	if err != nil || twice != canonical {
		t.Errorf("canonicalising a canonical URI gave %q, %v", twice, err)
	}

	// What it must not do is accept something that names no dataset.
	for _, bad := range []string{"", "   ", "gs://source-bucket/datasets/", "s3://"} {
		if _, err := CanonicalDatasetURI(bad); err == nil {
			t.Errorf("expected %q to be refused", bad)
		}
	}
}
