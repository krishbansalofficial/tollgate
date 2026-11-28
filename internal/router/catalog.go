package router

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Catalog maps public model names ("provider/model") to prices, and policy
// aliases ("tollgate/cheapest") to candidate lists.
type Catalog struct {
	Models  map[string]Price `yaml:"models"`
	Aliases map[string]Alias `yaml:"aliases"`
}

// Price is USD per million tokens.
type Price struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
	// Long is an optional tier for large prompts: a request whose prompt has
	// more than Long.OverInputTokens tokens is priced entirely at Long's rates
	// (input and output), as Anthropic bills Haiku 5.5.
	Long *LongPrice `yaml:"long,omitempty"`
}

type LongPrice struct {
	OverInputTokens int     `yaml:"over_input_tokens"`
	InputPerMTok    float64 `yaml:"input_per_mtok"`
	OutputPerMTok   float64 `yaml:"output_per_mtok"`
}

type Alias struct {
	Policy string   `yaml:"policy"` // cheapest | fastest | ordered
	Models []string `yaml:"models"`
}

func LoadCatalog(path string) (*Catalog, error) {
	if path == "" {
		return &Catalog{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("catalog %s: %w", path, err)
	}
	return &c, c.validate()
}

func (c *Catalog) validate() error {
	for name, p := range c.Models {
		if _, _, ok := split(name); !ok {
			return fmt.Errorf("catalog: model %q must be provider/model", name)
		}
		if p.Long != nil && p.Long.OverInputTokens <= 0 {
			return fmt.Errorf("catalog: model %q: long.over_input_tokens must be > 0", name)
		}
	}
	for name, a := range c.Aliases {
		switch a.Policy {
		case "cheapest", "fastest", "ordered":
		default:
			return fmt.Errorf("catalog: alias %q has unknown policy %q", name, a.Policy)
		}
		if len(a.Models) == 0 {
			return fmt.Errorf("catalog: alias %q has no models", name)
		}
		for _, m := range a.Models {
			if _, ok := c.Models[m]; !ok {
				return fmt.Errorf("catalog: alias %q references unknown model %q", name, m)
			}
		}
	}
	return nil
}

// Cost in USD for the given token counts; 0 for models not in the catalog.
func (c *Catalog) Cost(model string, in, out int) float64 {
	p := c.Models[model]
	inRate, outRate := p.InputPerMTok, p.OutputPerMTok
	if l := p.Long; l != nil && in > l.OverInputTokens {
		inRate, outRate = l.InputPerMTok, l.OutputPerMTok
	}
	return (float64(in)*inRate + float64(out)*outRate) / 1e6
}

func split(model string) (provider, native string, ok bool) {
	provider, native, ok = strings.Cut(model, "/")
	return provider, native, ok && provider != "" && native != ""
}
