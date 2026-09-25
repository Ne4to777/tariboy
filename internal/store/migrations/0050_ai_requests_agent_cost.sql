-- 0050_ai_requests_agent_cost.sql -- agent budget status sums an agent's
-- hour, day, week and month spend on every agent-list poll and proxied
-- request. With cost_usd in the index that sum reads only the index range for
-- the agent's current month instead of looking up each request row. The old
-- (agent, ts) index is a prefix of the new one and would only slow inserts.
CREATE INDEX idx_ai_requests_agent_ts_cost ON ai_requests(agent, ts, cost_usd);
DROP INDEX idx_ai_requests_agent;
