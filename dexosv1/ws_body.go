package dexosv1

type wsIdentity struct {
	Address    string  `json:"address"`
	Subaccount *uint32 `json:"subaccount"`
	Vault      *uint16 `json:"vault"`
}

type routeHeader struct {
	Symbol string `json:"symbol"`
	Kind   string `json:"kind"`
	wsIdentity
}

type frameBody interface{ header() routeHeader }

func (id wsIdentity) header() routeHeader { return routeHeader{wsIdentity: id} }

func (w *wirePrices) header() routeHeader { return routeHeader{} }

func (w *wireBook) header() routeHeader { return routeHeader{Symbol: w.Symbol, Kind: w.Kind} }

type wireBBO struct {
	Symbol string     `json:"symbol"`
	Kind   string     `json:"kind"`
	Bid    *[2]string `json:"bid"`
	Ask    *[2]string `json:"ask"`
	TS     int64      `json:"ts"`
}

func (w *wireBBO) header() routeHeader { return routeHeader{Symbol: w.Symbol, Kind: w.Kind} }

type wireTrade struct {
	ID     string `json:"id"`
	Symbol string `json:"symbol"`
	Kind   string `json:"kind"`
	Side   string `json:"side"`
	Price  string `json:"price"`
	Size   string `json:"size"`
	TS     int64  `json:"ts"`
}

func (w *wireTrade) header() routeHeader { return routeHeader{Symbol: w.Symbol, Kind: w.Kind} }

type ordersBody struct {
	wsIdentity
	wireOrderEvent
	Orders []wireOrder `json:"orders"`
}

type fillsBody struct {
	wsIdentity
	wireFill
}

type ledgerBody struct {
	wsIdentity
	Entries *[]LedgerEntry `json:"entries"`
}

type positionsBody struct {
	wsIdentity
	Positions []wirePosition `json:"positions"`
}

type accountBody struct {
	Account
	Subaccount *uint32 `json:"subaccount"`
	Epoch      *string `json:"epoch"`
	AsOfSeq    *uint64 `json:"asOfSeq"`
}

func (w *accountBody) header() routeHeader {
	return routeHeader{wsIdentity: wsIdentity{Address: w.Address, Subaccount: w.Subaccount, Vault: w.Vault}}
}

func newBody(channel string) frameBody {
	switch channel {
	case "prices":
		return &wirePrices{}
	case "book":
		return &wireBook{}
	case "bbo":
		return &wireBBO{}
	case "trades":
		return &wireTrade{}
	case "orders":
		return &ordersBody{}
	case "fills":
		return &fillsBody{}
	case "ledger":
		return &ledgerBody{}
	case "positions":
		return &positionsBody{}
	case "account":
		return &accountBody{}
	}
	return nil
}
