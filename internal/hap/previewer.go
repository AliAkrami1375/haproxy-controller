package hap

import "context"

// PreviewConfig renders and validates the configuration without applying it,
// adapting the Deployer to the ai.Previewer interface. It returns the rendered
// text, renderer warnings, HAProxy's validation output, and an error when the
// configuration is invalid.
func (d *Deployer) PreviewConfig(ctx context.Context) (string, []string, string, error) {
	res, out, err := d.Preview(ctx)
	if res == nil {
		return "", nil, out, err
	}
	return res.Content, res.Warnings, out, err
}
