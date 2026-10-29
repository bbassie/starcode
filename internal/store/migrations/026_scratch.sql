-- Projects starcode made for a thread started without one
-- (domain.ProjectAdded.Scratch): a folder of its own under the data
-- directory, kept out of the project lists.
ALTER TABLE projects ADD COLUMN scratch INTEGER NOT NULL DEFAULT 0;
