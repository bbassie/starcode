-- What the human answered when an approval was a question
-- (AskUserQuestion), as a JSON object of question text to answer.
ALTER TABLE approvals ADD COLUMN answers TEXT NOT NULL DEFAULT '';
