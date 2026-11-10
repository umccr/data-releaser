#!/usr/bin/env bash
#
# Exercises the whole direct-read flow against the cup_test dataset: copy it in, grant
# access, move the expiry, withdraw it, grant it again, destroy the copy, take that back,
# grant it once more, then destroy it for good and delete the objects.
#
# Several commands need an id that only exists once the one before them has run. Each share
# prints its grant id; paste it into the PASTE-GRANT-ID-HERE placeholders before running
# those lines, or run the script a section at a time.
#
# Redeploy DataReleaserStack before the first run: the copy-completion Lambda is built from
# functions/copycompletion and its code changed.

set -euo pipefail

# Check both ends and price the transfer without moving any bytes.
./data-releaser --config config.yaml stage s3 --dataset-uri s3://5180-guardians-node/cup_test/ --dry-run

# Start the real copy. Returns as soon as the transfer has been started, not when it ends.
./data-releaser --config config.yaml stage s3 --dataset-uri s3://5180-guardians-node/cup_test/

# Staging is asynchronous and nothing polls it, so wait for the copy to reach staged.
until ./data-releaser --config config.yaml list s3 | grep -q staged; do echo "waiting for the copy to be staged"; sleep 20; done

# The copy is in. Nothing is granted yet.
./data-releaser --config config.yaml list s3

# Prerequisites, key state, and the deployed policies against the ledger.
./data-releaser --config config.yaml validate s3

# Grant access. Prints the grant id the next two commands take.
./data-releaser --config config.yaml share s3 --dataset-uri s3://5180-guardians-node/cup_test/ --account 534840902377 --target-vpc-id vpc-031e3d24ea0e50664 --expiry 30d

# One copy, one grant.
./data-releaser --config config.yaml list s3

# The deployed policies should now match the ledger exactly.
./data-releaser --config config.yaml validate s3

# Move the expiry. Both the record and the time condition in the two documents move.
./data-releaser --config config.yaml extend s3 PASTE-GRANT-ID-HERE --duration 60d

# Withdraw the access. The objects and the key are untouched.
./data-releaser --config config.yaml revoke s3 PASTE-GRANT-ID-HERE

# Nothing granted, and the researcher's manifest is gone.
./data-releaser --config config.yaml list s3

# Grant it again. A NEW grant against the SAME copy: nothing is re-copied, and the withdrawn
# grant keeps its record. Two grant rows against one copy is the thing to look for.
./data-releaser --config config.yaml share s3 --dataset-uri s3://5180-guardians-node/cup_test/ --account 534840902377 --target-vpc-id vpc-031e3d24ea0e50664 --expiry 30d

./data-releaser --config config.yaml list s3

# Destroy the copy. Withdraws every live grant, then schedules deletion of the key. Asks for
# the dataset URI to be typed back; add --yes to skip that.
./data-releaser --config config.yaml purge s3 --dataset-uri s3://5180-guardians-node/cup_test/

# Every grant revoked, every copy purged, with the date the deletion becomes irreversible.
./data-releaser --config config.yaml list s3

# Take the purge back while its window is still open: the key deletion is cancelled, the key
# is enabled again, and the copy returns to staged. Nobody has access -- the grants purge
# withdrew stay withdrawn.
./data-releaser --config config.yaml restore s3 --dataset-uri s3://5180-guardians-node/cup_test/

# Staged again, with the revoked grants still on the record.
./data-releaser --config config.yaml list s3

# Grant it once more, to prove a restored copy is as grantable as a freshly staged one.
./data-releaser --config config.yaml share s3 --dataset-uri s3://5180-guardians-node/cup_test/ --account 534840902377 --target-vpc-id vpc-031e3d24ea0e50664 --expiry 30d

./data-releaser --config config.yaml validate s3

# Purge for good this time.
./data-releaser --config config.yaml purge s3 --dataset-uri s3://5180-guardians-node/cup_test/

# Delete the objects themselves. This buys storage, not access -- access ended at the purge.
# Irreversible with no window at all, unlike the key deletion it follows.
./data-releaser --config config.yaml delete s3 --dataset-uri s3://5180-guardians-node/cup_test/

# Copies deleted, grants revoked, bucket empty.
./data-releaser --config config.yaml list s3
