package main

import (
	"bytes"
	"compress/zlib"
	"testing"
)

// A pack like the bench push: blob(1 KiB) + tree + commit, plain objects.
func benchPackBytes() []byte {
	blob := bytes.Repeat([]byte("x"), 1024)
	blobObj := GitObject{Type: ObjBlob, Content: blob}
	blobSha := GitObjectID(ObjBlob, blob)
	tree := buildTree([]TreeEntry{{Mode: "100644", Name: "bench-data.txt", Sha: blobSha}})
	treeObj := GitObject{Type: ObjTree, Content: tree}
	commit := buildCommit(CommitInfo{
		Tree:      GitObjectID(ObjTree, tree),
		Parents:   nil,
		Author:    "bench <bench@bench> 1700000000 +0000",
		Committer: "bench <bench@bench> 1700000000 +0000",
		Message:   "bench: push 1\n",
	})
	commitObj := GitObject{Type: ObjCommit, Content: commit}
	return writePack([]GitObject{blobObj, treeObj, commitObj})
}

func BenchmarkReadPack(b *testing.B) {
	pack := benchPackBytes()
	res, err := readPack(pack, nil)
	if err != nil {
		b.Fatal(err)
	}
	if len(res.objects) != 3 {
		b.Fatalf("objects=%d", len(res.objects))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := readPack(pack, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInflateOne(b *testing.B) {
	raw := bytes.Repeat([]byte("x"), 1024)
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	comp := buf.Bytes()
	// pack entry: object header for blob size 1024, then the zlib stream.
	entry := append(encodeObjectHeader(3, 1024), comp...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := inflateOne(entry, len(entry)-len(comp)); err != nil {
			b.Fatal(err)
		}
	}
}
