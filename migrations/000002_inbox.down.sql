BEGIN;
DROP TABLE inbox_messages;
DROP FUNCTION check_inbox_completed();
DROP FUNCTION guard_inbox_completion();
COMMIT;
