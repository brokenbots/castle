-- 0018_refire_disposition: close evaluated runs' stories in the refire
-- candidate scan (KB-321). A terminal-failed run evaluated to a final
-- disposition (refire: successor minted; park: reclassified to the KB-226
-- awaiting_human shape; cosmetic / none: verdict recorded) carries a
-- non-NULL disposition stamp and ages out of the candidate scan, so
-- permanent no-action rows can never starve a fresh failure. Deferred and
-- budget-exhausted rows stay unstamped and revisitable — policy retunes
-- reach them on the next pass. Reopening a stamped row is a deliberate
-- data edit (UPDATE runs SET refire_disposition = NULL): the stamp is
-- data, not code state.

ALTER TABLE runs ADD COLUMN refire_disposition TEXT NULL;