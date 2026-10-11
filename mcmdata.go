package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// A McMaster product page fills itself from two of the site's own data calls
// (seen in recorded lookups), and both answer even when the page itself shows
// "Log in to view Product Detail":
//
//	/<mv…>/WebParts/OrderServer/ProductOrderInfo.aspx?partNumber=8336N108…
//	  {"partNumber":"8336N108","unitOfMeasure":"Pair","parentDescription":"Welding Mitts",
//	   "suffixDescription":"Cowhide Leather, Size L","pricingData":{"price":"$28.46 per pair",
//	   "priceLevels":[{"QuantityRangeMessage":"1-11 pairs","SellPrice":28.46},
//	                  {"QuantityRangeMessage":"12 or more","SellPrice":25.93}],"shouldShowPrice":true}}
//	/<mv…>/WebParts/Navigate/ProductContent.aspx?partNumber=8336N108…
//	  {"displayImage":{"sourcePath":"/mvD/Contents/gfx/ImageCache/833/8336n108-@100p_….png?ver=…"}}
//
// <mv…> is a site version segment (e.g. mv1791569618) that changes when
// McMaster deploys.

const (
	orderInfoPath = "/WebParts/OrderServer/ProductOrderInfo.aspx"
	contentPath   = "/WebParts/Navigate/ProductContent.aspx"
)

var mvSegmentRe = regexp.MustCompile(`/(mv\d+)/`)

type orderInfo struct {
	PartNumber        string `json:"partNumber"`
	UnitOfMeasure     string `json:"unitOfMeasure"`
	ParentDescription string `json:"parentDescription"`
	SuffixDescription string `json:"suffixDescription"`
	PricingData       *struct {
		Price           string `json:"price"`
		ShouldShowPrice bool   `json:"shouldShowPrice"`
		PriceLevels     []struct {
			QuantityRangeMessage string  `json:"QuantityRangeMessage"`
			SellPrice            float64 `json:"SellPrice"`
		} `json:"priceLevels"`
	} `json:"pricingData"`
}

type productContent struct {
	DisplayImage *struct {
		SourcePath string `json:"sourcePath"`
	} `json:"displayImage"`
}

var (
	rangeMsgRe  = regexp.MustCompile(`^\s*([\d,]+)\s*[-–]\s*([\d,]+)`)
	openMsgRe   = regexp.MustCompile(`^\s*([\d,]+)\s*(?:or more|and (?:over|up)|\+)`)
	singleMsgRe = regexp.MustCompile(`^\s*([\d,]+)\b`)
	priceTextRe = regexp.MustCompile(`\$\s?([\d,]+\.\d{2})`)
)

func atoiComma(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}

// parseOrderInfo turns a ProductOrderInfo answer into a Part (no image yet).
// It reports false when the answer is not product data for pn.
func parseOrderInfo(body, pn string) (*Part, bool) {
	var oi orderInfo
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &oi) != nil || !strings.EqualFold(oi.PartNumber, pn) {
		return nil, false
	}
	parent, suffix := strings.TrimSpace(oi.ParentDescription), strings.TrimSpace(oi.SuffixDescription)
	name := strings.Trim(strings.Join([]string{parent, suffix}, ", "), ", ")
	if name == "" {
		return nil, false
	}
	p := &Part{PartNumber: pn, Name: name, Unit: strings.TrimSpace(oi.UnitOfMeasure), Source: "auto"}
	for _, n := range []string{name, parent, suffix} {
		if n != "" && !slices.Contains(p.NameOptions, n) {
			p.NameOptions = append(p.NameOptions, n)
		}
	}
	if pd := oi.PricingData; pd != nil && pd.ShouldShowPrice {
		for _, lv := range pd.PriceLevels {
			if lv.SellPrice <= 0 {
				continue
			}
			t := PriceTier{Min: 1, Price: round2(lv.SellPrice)}
			msg := lv.QuantityRangeMessage
			switch {
			case rangeMsgRe.MatchString(msg):
				m := rangeMsgRe.FindStringSubmatch(msg)
				t.Min, t.Max = atoiComma(m[1]), atoiComma(m[2])
			case openMsgRe.MatchString(msg):
				t.Min = atoiComma(openMsgRe.FindStringSubmatch(msg)[1])
			case singleMsgRe.MatchString(msg):
				t.Min = atoiComma(singleMsgRe.FindStringSubmatch(msg)[1])
				t.Max = t.Min
			}
			if t.Min < 1 {
				t.Min = 1
			}
			p.Tiers = append(p.Tiers, t)
		}
		sort.SliceStable(p.Tiers, func(i, j int) bool { return p.Tiers[i].Min < p.Tiers[j].Min })
		if len(p.Tiers) == 0 {
			if m := priceTextRe.FindStringSubmatch(pd.Price); m != nil {
				if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil && v > 0 {
					p.Tiers = []PriceTier{{Min: 1, Price: round2(v)}}
				}
			}
		}
		if n := len(p.Tiers); n > 0 {
			p.Tiers[0].Min = 1
			p.Tiers[n-1].Max = 0 // the last level has no ceiling
		}
	}
	return p, true
}

// parseImagePath returns the product image path from a ProductContent answer.
func parseImagePath(body string) string {
	var pc productContent
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &pc) != nil || pc.DisplayImage == nil {
		return ""
	}
	return strings.TrimSpace(pc.DisplayImage.SourcePath)
}
