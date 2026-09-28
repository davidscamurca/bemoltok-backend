package main

import "testing"

func TestParseSkuImageURLCamelCase(t *testing.T) {
	body := []byte(`{
		"productRefId": "123",
		"imageUrl": "https://cdn.example/photo.jpg",
		"images": [{"imageUrl": "https://cdn.example/other.jpg"}]
	}`)
	got := parseSkuImageURL(body)
	if got != "https://cdn.example/photo.jpg" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeVtexImageURLRelative(t *testing.T) {
	got := normalizeVtexImageURL("/arquivos/ids/1/a.jpg")
	want := "https://bemol.vteximg.com.br/arquivos/ids/1/a.jpg"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
