package config

import (
	"context"
	"encoding/json"
	"fmt"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
)

type Source interface {
	Values(context.Context) (map[string]interface{}, error)
}

func SourceFromClusterExtension(ext *ocv1.ClusterExtension) Source {
	if ext.Spec.Config == nil {
		return &defaultSource{}
	}

	cfg := ext.Spec.Config
	switch cfg.Type {
	case ocv1.ConfigSourceTypeInline:
		return &InlineSource{Data: cfg.Inline.Raw}
	default:
		panic(fmt.Sprintf("unknown values source type %q", cfg.Type))
	}
}

type defaultSource struct{}

func (*defaultSource) Values(context.Context) (map[string]interface{}, error) {
	return nil, nil
}

type InlineSource struct {
	Data []byte
}

func (p *InlineSource) Values(_ context.Context) (map[string]interface{}, error) {
	values := map[string]interface{}{}
	if err := json.Unmarshal(p.Data, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal inlined configuration: %w", err)
	}
	return values, nil
}
