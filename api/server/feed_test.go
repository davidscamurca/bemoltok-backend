package main

import "fmt"
import "testing"

func TestInterleaveFeedRatio(t *testing.T) {
	products := make([]feedItem, 0, 8)
	for i := 0; i < 8; i++ {
		products = append(products, feedItem{Type: "product", ProductID: fmt.Sprintf("p%d", i)})
	}
	posts := []eligiblePost{
		{doc: PostDoc{ID: "post1", UID: "u1", ProductID: "px"}, eligibility: "pool"},
		{doc: PostDoc{ID: "post2", UID: "u2", ProductID: "py"}, eligibility: "pool"},
	}
	mixed := interleaveFeed(products, posts, 4)
	if len(mixed) != 10 {
		t.Fatalf("len=%d", len(mixed))
	}
	postCount := 0
	for _, it := range mixed {
		if it.Type == "post" {
			postCount++
		}
	}
	if postCount != 2 {
		t.Fatalf("posts=%d", postCount)
	}
}

func TestInterleaveFeedNoConsecutiveSameAuthor(t *testing.T) {
	products := make([]feedItem, 0, 12)
	for i := 0; i < 12; i++ {
		products = append(products, feedItem{Type: "product", ProductID: "p"})
	}
	posts := []eligiblePost{
		{doc: PostDoc{ID: "a1", UID: "same", ProductID: "p1"}},
		{doc: PostDoc{ID: "a2", UID: "same", ProductID: "p2"}},
		{doc: PostDoc{ID: "a3", UID: "other", ProductID: "p3"}},
	}
	mixed := interleaveFeed(products, posts, 4)
	var prevPostAuthor string
	for _, it := range mixed {
		if it.Type != "post" {
			continue
		}
		if prevPostAuthor != "" && prevPostAuthor == it.AuthorUID {
			t.Fatalf("consecutive same author")
		}
		prevPostAuthor = it.AuthorUID
	}
}
