-- Adds command_json, args_json, and (if missing) container_port columns to
-- the instances table. Persisting Command/Args/ContainerPort lets Start()
-- correctly recreate Pods after Stop, which is mandatory for OpenClaw
-- instances whose entrypoint is overridden by new-yunwu-api with a
-- supervisor script.

SET @instance_container_port_column_exists = (
  SELECT COUNT(*)
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'instances'
    AND COLUMN_NAME = 'container_port'
);
SET @instance_container_port_column_sql = IF(
  @instance_container_port_column_exists = 0,
  'ALTER TABLE instances ADD COLUMN container_port INT NOT NULL DEFAULT 0 AFTER image_tag',
  'SELECT 1'
);
PREPARE instance_container_port_column_stmt FROM @instance_container_port_column_sql;
EXECUTE instance_container_port_column_stmt;
DEALLOCATE PREPARE instance_container_port_column_stmt;

SET @instance_command_json_column_exists = (
  SELECT COUNT(*)
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'instances'
    AND COLUMN_NAME = 'command_json'
);
SET @instance_command_json_column_sql = IF(
  @instance_command_json_column_exists = 0,
  'ALTER TABLE instances ADD COLUMN command_json LONGTEXT NULL AFTER container_port',
  'SELECT 1'
);
PREPARE instance_command_json_column_stmt FROM @instance_command_json_column_sql;
EXECUTE instance_command_json_column_stmt;
DEALLOCATE PREPARE instance_command_json_column_stmt;

SET @instance_args_json_column_exists = (
  SELECT COUNT(*)
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'instances'
    AND COLUMN_NAME = 'args_json'
);
SET @instance_args_json_column_sql = IF(
  @instance_args_json_column_exists = 0,
  'ALTER TABLE instances ADD COLUMN args_json LONGTEXT NULL AFTER command_json',
  'SELECT 1'
);
PREPARE instance_args_json_column_stmt FROM @instance_args_json_column_sql;
EXECUTE instance_args_json_column_stmt;
DEALLOCATE PREPARE instance_args_json_column_stmt;
