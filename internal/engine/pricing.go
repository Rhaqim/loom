package engine

// pricing.go provides per-model cost calculation. Generators report their model
// id on Result metadata under "model"; the engine looks the model up in
// Config.Pricing and falls back to DefaultPrice (or a built-in flat rate).

// ModelPrice is the USD price per one million tokens for a model.
type ModelPrice struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Cost returns the USD cost for the given token counts.
func (p ModelPrice) Cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.InputPer1M/1_000_000 +
		float64(outputTokens)*p.OutputPer1M/1_000_000
}

// defaultModelPrice is the flat fallback ($1 in / $3 out per 1M) used when no
// pricing is configured and a model is unknown.
var defaultModelPrice = ModelPrice{InputPer1M: 1.0, OutputPer1M: 3.0}

// priceFor returns the price for a model id and whether that price came from
// real configuration (Config.Pricing or Config.DefaultPrice) rather than the
// built-in flat rate.
//
// The bool matters: token counts are always exact, but with no pricing
// configured the USD figures are fabricated from defaultModelPrice. Callers
// that surface money — or enforce budgets with it — must be able to tell the
// difference between a real price and a placeholder.
func (e *Engine) priceFor(model string) (ModelPrice, bool) {
	if e.pricing != nil {
		if p, ok := e.pricing[model]; ok {
			return p, true
		}
	}
	if e.defaultPrice != nil {
		return *e.defaultPrice, true
	}
	return defaultModelPrice, false
}

// PriceResult returns the USD cost for a result's token usage under the model's
// configured pricing. model is the generator's reported model id ("" uses the
// default price). Exposed so applications can compute per-turn spend consistently
// with what the engine records.
//
// The second return reports whether the price was configured. When false the
// USD figure is a placeholder derived from a built-in flat rate — render it as
// unknown rather than as money.
func (e *Engine) PriceResult(model string, inputTokens, outputTokens int) (float64, bool) {
	p, known := e.priceFor(model)
	return p.Cost(inputTokens, outputTokens), known
}

// ResultModel returns the model id a generator reported on a result's metadata,
// or "" if none.
func ResultModel(r Result) string {
	if r == nil {
		return ""
	}
	if m, ok := r.Metadata()["model"].(string); ok {
		return m
	}
	return ""
}
