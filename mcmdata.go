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
	readChoices(body, p)
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

// ---------- options chosen inside one part number ----------
//
// Some products ask for choices on the same part number ("Choose a Length,
// ft." 25 / 100 / 1,000, "Choose a Color" Black / Blue …; recorded for
// 7856K41 on 2026-10-16). ProductOrderInfo lists them under easyToOrderModel,
// with no price until a choice is made, plus the hidden part numbers behind
// the combinations ("terminalComponentPartNumbersInScope", one per
// combination). Each of those answers ProductOrderInfo with its own price.

type richText struct {
	FullText string `json:"fullText"`
}

type specParam struct {
	DisplayName  richText          `json:"displayName"`
	IsSelected   bool              `json:"isSelected"`
	AttrValueIDs []json.RawMessage `json:"attrValueIds"`
}

type specChoice struct {
	DisplayName   richText    `json:"displayName"`
	AttributeName richText    `json:"attributeName"`
	Params        []specParam `json:"specChoiceParameters"`
}

type easyToOrder struct {
	EasyToOrderModel *struct {
		Component  []json.RawMessage `json:"componentSpecSpecChoices"`
		Enumerated []json.RawMessage `json:"enumeratedRangeSpecChoices"`
		TextInput  []json.RawMessage `json:"textInputSpecChoices"`
		Implicit   []json.RawMessage `json:"implicitlySelectedAttrValueIds"`
		Resolved   string            `json:"resolvedComponentPartNumber"`
		Terminal   []string          `json:"terminalComponentPartNumbersInScope"`
	} `json:"easyToOrderModel"`
}

func rawID(r json.RawMessage) string { return strings.Trim(strings.TrimSpace(string(r)), `"`) }

// readChoices adds the product's choices, the hidden part numbers behind
// them, and (for one of those) which options it stands for. Each group is
// read on its own, so one oddly shaped group never breaks the rest.
func readChoices(body string, p *Part) {
	var e easyToOrder
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &e) != nil || e.EasyToOrderModel == nil {
		return
	}
	m := e.EasyToOrderModel
	for _, r := range m.Implicit {
		p.implicitIDs = append(p.implicitIDs, rawID(r))
	}
	p.selected = map[string]string{}
	groups := [][]json.RawMessage{m.Component, m.Enumerated, m.TextInput}
	for gi, list := range groups {
		for _, raw := range list {
			var sc specChoice
			if json.Unmarshal(raw, &sc) != nil {
				continue
			}
			c := Choice{Label: strings.TrimSpace(sc.DisplayName.FullText), Name: strings.TrimSpace(sc.AttributeName.FullText)}
			if c.Name == "" {
				c.Name = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.Label, "Choose a "), "Choose an "))
			}
			if c.Label == "" {
				c.Label = c.Name
			}
			if c.Name == "" {
				continue
			}
			for _, sp := range sc.Params {
				t := strings.TrimSpace(sp.DisplayName.FullText)
				if t == "" {
					continue
				}
				o := ChoiceOption{Text: t}
				for _, id := range sp.AttrValueIDs {
					o.IDs = append(o.IDs, rawID(id))
				}
				c.Options = append(c.Options, o)
				if sp.IsSelected {
					p.selected[c.Name] = t
				}
			}
			c.Free = len(c.Options) == 0 && gi == 2 // typed in (e.g. a cut length)
			if len(c.Options) > 0 || c.Free {
				p.Choices = append(p.Choices, c)
			}
		}
	}
	if m.Resolved == "" || strings.EqualFold(m.Resolved, p.PartNumber) {
		for _, t := range m.Terminal {
			if t = strings.ToUpper(strings.TrimSpace(t)); t != "" && t != p.PartNumber {
				p.components = append(p.components, t)
			}
		}
	}
}

// variantValues says which option of each choice a hidden part number stands
// for: from its own answer (an option marked selected, or the option ids it
// carries), else from its description ("… 100 ft. Long, Black"). False when
// any choice stays unclear.
func variantValues(choices []Choice, comp *Part) (map[string]string, bool) {
	vals := map[string]string{}
	ids := map[string]bool{}
	for _, id := range comp.implicitIDs {
		ids[id] = true
	}
	desc := normText(comp.Name + " " + strings.Join(comp.NameOptions, " ") + " " + comp.Unit)
	for _, c := range choices {
		if c.Free {
			continue
		}
		if v := comp.selected[c.Name]; v != "" {
			vals[c.Name] = v
			continue
		}
		var byID, byText []string
		for _, o := range c.Options {
			for _, id := range o.IDs {
				if ids[id] {
					byID = append(byID, o.Text)
					break
				}
			}
			if containsWord(desc, normText(o.Text)) {
				byText = append(byText, o.Text)
			}
		}
		switch {
		case len(byID) == 1:
			vals[c.Name] = byID[0]
		case len(byText) == 1:
			vals[c.Name] = byText[0]
		default:
			return nil, false
		}
	}
	return vals, true
}

func normText(s string) string { return strings.ToLower(strings.ReplaceAll(s, ",", "")) }

// containsWord: w appears in s not as part of a longer number or word
// ("100" is not in "1000" or "1.100", "red" is not in "colored").
func containsWord(s, w string) bool {
	if w == "" {
		return false
	}
	isWordChar := func(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' }
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(w)
		before := a == 0 || !isWordChar(s[a-1]) && !(s[a-1] == '.' && a >= 2 && s[a-2] >= '0' && s[a-2] <= '9')
		after := b == len(s) || !isWordChar(s[b]) && !(s[b] == '.' && b+1 < len(s) && s[b+1] >= '0' && s[b+1] <= '9')
		if before && after {
			return true
		}
		i = a + 1
	}
}
