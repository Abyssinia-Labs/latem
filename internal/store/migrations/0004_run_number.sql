-- 0004_run_number: every run gets a monotonic number (amendment A36).
--
-- App Store Connect refuses a build number it has already accepted for a
-- marketing version, so a project with CURRENT_PROJECT_VERSION pinned uploads
-- exactly once and fails forever after — having first archived, exported and
-- uploaded. The reference jobs work around it by defaulting
-- LATEM_BUILD_NUMBER to Unix time, which is unique and says nothing: a build in
-- TestFlight cannot be traced back to the run that produced it, which is most
-- of the point of keeping §22 at all.
--
-- A number rather than the run id because the id is a random token and a build
-- number is `a period-separated list of at most three non-negative integers`.
--
-- # Why a sequence table rather than MAX(number) + 1
--
-- Retention deletes old runs. A number derived from the surviving rows is
-- therefore not monotonic across a prune: delete the highest-numbered run and
-- the next one reissues its number, which is precisely the value App Store
-- Connect has already accepted and will reject. sqlite's implicit rowid has the
-- same defect for the same reason — without AUTOINCREMENT it reuses the largest
-- deleted value. The sequence survives both because nothing prunes it: it is
-- one row holding the highest number ever issued on this machine.
--
-- # What this number is not
--
-- Unique across machines. It counts runs on *this* agent, so two machines
-- building the same app both issue 1, 2, 3 and collide at upload. That is a
-- deployment question rather than a schema one — release on one machine, or
-- override LATEM_BUILD_NUMBER in the job — and §18.1 states it where an
-- operator will meet it.

ALTER TABLE runs ADD COLUMN number INTEGER NOT NULL DEFAULT 0;

CREATE TABLE sequences (
  name  TEXT PRIMARY KEY,
  value INTEGER NOT NULL   -- the highest value ever issued, never decremented
);

-- Existing history is numbered in creation order, so a machine that has been
-- building for a month keeps an ordering that means something rather than a
-- column of zeroes. (created_at, id) is the same total order List pages by:
-- created_at alone is not unique, and a tie broken arbitrarily would give two
-- runs the same number.
UPDATE runs SET number = (
  SELECT COUNT(*) FROM runs AS earlier
   WHERE earlier.created_at < runs.created_at
      OR (earlier.created_at = runs.created_at AND earlier.id <= runs.id)
);

-- Seeded past the backfill, so the first run after the upgrade does not reissue
-- a number an upload already carries.
INSERT INTO sequences (name, value)
  VALUES ('run_number', (SELECT COALESCE(MAX(number), 0) FROM runs));
