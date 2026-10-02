-- The quiet exit code of a recurring script is now the protocol constant 111,
-- not a per-definition setting. A definition stored with another code keeps
-- working: its command is wrapped in a nested shell that reports that code as
-- 111 and passes every other exit code through. The text matches
-- script.LegacyQuietCommand; change both together.
--
-- The column stays, always NULL. Dropping it needs a rebuild of scripts, and
-- a rebuild under enforced foreign keys would cascade into script_runs.

UPDATE scripts
SET command = 'sh -c ''' || replace(command, '''', '''\''''') || '''' || char(10)
           || '__tariboy_rc=$?' || char(10)
           || '[ "$__tariboy_rc" -eq ' || quiet_exit || ' ] && exit 111' || char(10)
           || 'exit "$__tariboy_rc"'
WHERE quiet_exit IS NOT NULL AND quiet_exit <> 111;

UPDATE scripts SET quiet_exit = NULL WHERE quiet_exit IS NOT NULL;
