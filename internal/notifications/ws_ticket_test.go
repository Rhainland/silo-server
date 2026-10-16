package notifications

import (
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// The Redis store keeps the profile token beside the ticket, leaving the
// ticket value in the format older nodes read, and consumes a ticket an older
// node minted as one without PIN proof.
func TestRedisTicketStoreKeepsProfileToken(t *testing.T) {
	endpoint := os.Getenv("SILO_TEST_REDIS_URL")
	if endpoint == "" {
		t.Skip("SILO_TEST_REDIS_URL required for the Redis ticket store")
	}
	opts, err := redis.ParseURL(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	store := NewTicketStore(client)

	minted := Ticket{UserID: 7, ProfileID: "profile-1", ProfileToken: "proof"}
	ticket, _, err := store.Mint(t.Context(), minted)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := client.Get(t.Context(), redisTicketPrefix+ticket).Result(); err != nil || value != "7|profile-1" {
		t.Fatalf("ticket value = %q, %v; want the format older nodes read", value, err)
	}
	if binding, ok := store.Consume(t.Context(), ticket); !ok || binding != minted {
		t.Fatalf("consume returned %+v %v", binding, ok)
	}
	if _, ok := store.Consume(t.Context(), ticket); ok {
		t.Fatal("tickets must be single-use")
	}

	if err := client.Set(t.Context(), redisTicketPrefix+"legacy-fixture", "7|profile-1", TicketTTL).Err(); err != nil {
		t.Fatal(err)
	}
	if binding, ok := store.Consume(t.Context(), "legacy-fixture"); !ok || binding != (Ticket{UserID: 7, ProfileID: "profile-1"}) {
		t.Fatalf("legacy value consumed as %+v %v", binding, ok)
	}
}
