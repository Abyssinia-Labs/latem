-- 0002_approval: record what a local `latem approve` actually approved (A27).
--
-- INV-5 requires a local approval for a ref outside allowed_refs, and 0001 had
-- nowhere to write one. The consequence was not a missing convenience: gate 8
-- re-evaluated the policy on every attempt, found the same ref still outside it,
-- and parked the run again — so an approved run looped forever, paying for a
-- mirror fetch each time round.
--
-- Approval is bound to a commit rather than to a ref, and that is the whole
-- point of these two columns. A ref can point at a branch a contributor
-- controls; approving the *ref* would let a benign commit be approved and a
-- force-push swap in a hostile one before the run builds. approved_sha pins the
-- commit the operator actually said yes to, and gate 8 admits the run only when
-- the freshly resolved commit still equals it.
--
-- Both columns are nullable with no default, so this applies forward over an
-- existing history: every run already in the table reads as "never approved",
-- which is exactly what it is.

ALTER TABLE runs ADD COLUMN approved_at   INTEGER;  -- local `latem approve` (INV-5)
ALTER TABLE runs ADD COLUMN approved_sha  TEXT;     -- the commit that approval pinned
