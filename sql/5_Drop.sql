
-- drop unnecessary column
ALTER TABLE isu_condition
  DROP COLUMN `condition`,
  DROP COLUMN `level`;
