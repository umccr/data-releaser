package dataset

// Reading a dataset's objects from a list the operator supplies.
//
// The default is to hand a wildcard to the copy service and let it enumerate the dataset
// with its own credentials, which is what keeps the source store's credentials out of
// this tool entirely. That default has a ceiling: the copy service expands one wildcard
// to a fixed maximum number of objects and refuses the whole run above it, because the
// result of heading that many objects is what has to fit in one step's payload. A named
// object carries no such limit, since each is headed on its own.
//
// So a dataset larger than that ceiling is copied by naming its objects, and naming them
// is the operator's to do: they hold the credentials to list the source, and this tool
// deliberately does not. What arrives here is that list.
//
// The keys are checked against the dataset they claim to belong to rather than trusted.
// A list is produced by hand, from a shell pipeline, against a store this code cannot
// see -- so a key from a neighbouring folder, or from another bucket, is a plausible
// mistake, and one that would otherwise copy data into a cache bucket whose copy record
// does not describe it.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxObjectListBytes bounds how much of a list is read.
//
// A dataset of any plausible size fits far inside this; what it refuses is a file handed
// over by mistake -- a BAM, a log, a whole listing of the bucket -- which would otherwise
// be read into memory in full before anything noticed it was not a list of keys.
const maxObjectListBytes = 64 << 20

// ParseObjectList reads the objects of one dataset from a list of keys, one per line.
//
// Blank lines and lines beginning with "#" are ignored, so a list can carry a note about
// where it came from. Each remaining line is one object key, written either as a full
// s3:// URI or as a key within the source bucket -- both of the forms listing a bucket
// produces -- and each is resolved to the key relative to the dataset's own prefix, which
// is what a copy instruction carries.
//
// The result is in the order given. Ordering and duplicate detection belong to the
// instruction builder, which applies them to every object list however it was obtained.
func ParseObjectList(datasetURI string, r io.Reader) ([]SourceObject, error) {
	source, err := ParseDatasetURI(datasetURI)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(io.LimitReader(r, maxObjectListBytes))
	// A generous line, since an object key may be long, but bounded: the default would
	// refuse a legitimate key on a deeply nested dataset.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		objects []SourceObject
		errs    []error
		lineNo  int
	)
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, err := relativeObjectKey(source, line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", lineNo, err))
			continue
		}
		objects = append(objects, SourceObject{Key: key})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the object list: %w", err)
	}

	// Every bad line is reported at once. A list is assembled in bulk, so its mistakes
	// come in bulk too, and fixing them one run at a time is no way to spend an hour.
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf(
			"the object list names no objects; a dataset copied from an empty list would be "+
				"recorded as staged having delivered nothing. Omit the list to have %q enumerated "+
				"by the copy service instead", datasetURI)
	}
	return objects, nil
}

// relativeObjectKey resolves one line of a list to the object's key relative to the
// dataset's prefix.
func relativeObjectKey(source DatasetLocation, line string) (string, error) {
	key := line
	if scheme, rest, found := strings.Cut(line, "://"); found {
		if !strings.EqualFold(scheme, "s3") {
			return "", fmt.Errorf("%q has scheme %q; only s3 is understood", line, scheme)
		}
		bucket, remainder, _ := strings.Cut(rest, "/")
		if bucket != source.Bucket {
			return "", fmt.Errorf(
				"%q is in bucket %q, and this dataset is in %q; an object outside the dataset "+
					"cannot be copied as part of it", line, bucket, source.Bucket)
		}
		key = remainder
	}

	key = strings.TrimPrefix(key, "/")
	if key == "" {
		return "", fmt.Errorf("%q names no object", line)
	}

	// Written as a key within the bucket, which is the form a listing gives, so the
	// dataset's own prefix leads it and comes off here. A key not under that prefix
	// belongs to another dataset or to none.
	relative, under := strings.CutPrefix(key, source.Prefix)
	if !under {
		return "", fmt.Errorf(
			"%q is not under the dataset's prefix %q, so it is not part of this dataset",
			line, source.Prefix)
	}
	if relative == "" {
		return "", fmt.Errorf("%q names the dataset's own prefix rather than an object in it", line)
	}
	return relative, nil
}
