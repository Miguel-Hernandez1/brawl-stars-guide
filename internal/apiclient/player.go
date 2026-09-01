package apiclient

import (
	"context"
	"fmt"
)

// GetPlayer fetches the profile for the given player tag.
// tag may include or omit the leading '#'.
func (c *Client) GetPlayer(ctx context.Context, tag string) (*PlayerResponse, error) {
	path := fmt.Sprintf("/players/%s", encodedTag(tag))
	var resp PlayerResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("get player %s: %w", tag, err)
	}
	return &resp, nil
}

// GetBattleLog fetches the most recent battles for the given player tag.
// The API returns at most 25 battles. Older battles are not accessible.
func (c *Client) GetBattleLog(ctx context.Context, tag string) (*BattleLogResponse, error) {
	path := fmt.Sprintf("/players/%s/battlelog", encodedTag(tag))
	var resp BattleLogResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("get battle log %s: %w", tag, err)
	}
	return &resp, nil
}
