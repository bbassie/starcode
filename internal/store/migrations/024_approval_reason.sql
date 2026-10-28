-- Why the agent asked: Claude's decision_reason, for an ask that its
-- own safety checks or an ask rule raised (see agent.ApprovalRequested).
ALTER TABLE approvals ADD COLUMN reason TEXT NOT NULL DEFAULT '';
