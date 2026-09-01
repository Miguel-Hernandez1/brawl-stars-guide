package apiclient

import (
	"context"
	"fmt"
)

// GetBrawlers fetches the full brawler list from /brawlers.
// Used to seed the brawlers reference table.
func (c *Client) GetBrawlers(ctx context.Context) (*BrawlerListResponse, error) {
	var resp BrawlerListResponse
	if err := c.get(ctx, "/brawlers", &resp); err != nil {
		return nil, fmt.Errorf("get brawlers: %w", err)
	}
	return &resp, nil
}
