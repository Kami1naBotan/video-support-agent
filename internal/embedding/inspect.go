package embedding

import (
	"context"
	"time"
)

func (c *Client) InspectIdentity(ctx context.Context) (ModelIdentity, error) {
	if c == nil {
		return ModelIdentity{}, ErrInput
	}

	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	identity, err := c.readIdentity(bounded)
	if err != nil {
		return ModelIdentity{}, err
	}
	if err := bounded.Err(); err != nil {
		return ModelIdentity{}, err
	}
	return identity, nil
}
