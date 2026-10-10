package a2ashape

// IsWaitCode reports whether code is one of the waits of ux §1.4 C2 — a
// message or request held back on this node or at a hub for a reason that
// passes by itself — whose short phrase delivery_unconfirmed.waiting and
// card_price_unavailable's {why} use.
func IsWaitCode(code string) bool {
	_, ok := phraseSets["wait"][code]
	return ok
}

// ChargeText is a card's charge (free, partial, paid; HPP PAY-C15) in a
// language, as the card header and the find table show it: "免费",
// "部分收费", "收费". "" for any other value.
func ChargeText(charge string, lang Language) string {
	p, ok := phraseSets["charge"][charge]
	if !ok {
		return ""
	}
	return p.in(lang)
}
