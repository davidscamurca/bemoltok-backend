package main

import (
	"encoding/json"
	"testing"
)

func TestSynthesizeSKUFromSearch(t *testing.T) {
	body := []byte(`[
	  {
	    "productId": "56428",
	    "productName": "Agua Sanitaria",
	    "productReference": "256922",
	    "link": "https://bemol.vtexcommercestable.com.br/agua-sanitaria-brinort-1-litro/p",
	    "brand": "Brinort",
	    "items": [
	      {
	        "itemId": "56430",
	        "referenceId": [{"Key": "RefId", "Value": "256922"}],
	        "images": [{"imageUrl": "http://bemol.vteximg.com.br/arquivos/ids/1/256922.jpg"}],
	        "sellers": [
	          {
	            "sellerId": "1",
	            "commertialOffer": {
	              "Price": 19.90,
	              "ListPrice": 24.90
	            }
	          }
	        ]
	      }
	    ]
	  }
	]`)
	out, ok := synthesizeSKUFromSearch(body, "256922")
	if !ok {
		t.Fatal("expected match")
	}
	var sku map[string]interface{}
	if err := json.Unmarshal(out, &sku); err != nil {
		t.Fatal(err)
	}
	if sku["ProductRefId"] != "256922" {
		t.Fatalf("ref: %v", sku["ProductRefId"])
	}
	if sku["DetailUrl"] != "/agua-sanitaria-brinort-1-litro/p" {
		t.Fatalf("detail: %v", sku["DetailUrl"])
	}
	img, _ := sku["ImageUrl"].(string)
	if img == "" || img[:8] != "https://" {
		t.Fatalf("image: %v", img)
	}
	p := parseSkuPrice(out)
	if p != 19.90 {
		t.Fatalf("expected price 19.90, got %v", p)
	}
	if _, ok := synthesizeSKUFromSearch(body, "51802"); ok {
		t.Fatal("unrelated ref must not match")
	}
}

func TestMergeSKUVisualsPreservesAndEnrichesPrice(t *testing.T) {
	skuWithoutPrice := []byte(`{
		"ProductId": 56428,
		"NameComplete": "Agua Sanitaria",
		"DetailUrl": "/agua/p",
		"ImageUrl": "https://img.jpg"
	}`)
	synthesizedWithPrice := []byte(`{
		"ProductId": 56428,
		"Sellers": [
			{
				"commertialOffer": {
					"Price": 15.50,
					"ListPrice": 18.00
				}
			}
		]
	}`)
	merged := mergeSKUVisuals(skuWithoutPrice, synthesizedWithPrice)
	price := parseSkuPrice(merged)
	if price != 15.50 {
		t.Fatalf("expected merged price 15.50, got %v", price)
	}
}
