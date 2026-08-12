package imapserver

import "context"

func handleIdle(ctx context.Context, c *conn, command *queuedCommand) error {
	if err := c.idleUntilDone(ctx); err != nil {
		return err
	}
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", "IDLE completed")
}
