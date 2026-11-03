//go:build unix

package main

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"testing"
)

func TestListDirs(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "b", "A", "c/x", ".git", "@eaDir", "#recycle")
	if got := listDirs(dir); !reflect.DeepEqual(got, []string{"A", "b", "c"}) {
		t.Fatalf("listDirs = %v", got)
	}
	if !hasKids(dir) || hasKids(dir+"/b") || !hasKids(dir+"/c") {
		t.Fatal("hasKids wrong")
	}
}

func displays(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Display)
	}
	sort.Strings(out)
	return out
}

func TestWalk(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "a/b/c/d/e", "node_modules/x", ".hidden/y", "@eaDir/z", "#recycle/q", "keep/node_modules", "m")
	bases := []Base{{Dir: dir, Label: "r", Self: true}}
	got := displays(collect(walk(context.Background(), bases, 3, defaultIgnore)))
	want := []string{"r", "r/a", "r/a/b", "r/a/b/c", "r/keep", "r/m"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("walk = %v\nwant %v", got, want)
	}
	got = displays(collect(walk(context.Background(), []Base{{Dir: dir + "/a", Label: "r/a"}}, 2, nil)))
	if want := []string{"r/a/b", "r/a/b/c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subtree walk = %v, want %v", got, want)
	}
}

func TestRank(t *testing.T) {
	entries := []Entry{
		{Path: "/1", Display: "w/alpha/x"},
		{Path: "/2", Display: "w/alpha"},
		{Path: "/3", Display: "w/zzz-a-l-p-h-a"},
		{Path: "/4", Display: "w/beta"},
	}
	var got []string
	for _, r := range rank(entries, "alpha", map[string]bool{"/3": true}) {
		got = append(got, r.Display)
	}
	if want := []string{"w/zzz-a-l-p-h-a", "w/alpha", "w/alpha/x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rank = %v, want %v", got, want)
	}
	all := rank(entries, "", map[string]bool{"/1": true})
	if len(all) != 4 || all[0].Display != "w/alpha/x" || all[1].Display != "w/beta" {
		t.Fatalf("empty query order: %v", all)
	}
	if r := rank(entries, "ALP", nil); !slices.ContainsFunc(r, func(r Result) bool { return r.Display == "w/alpha" }) {
		t.Fatal("match must be case-insensitive")
	}
}
