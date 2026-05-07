-- Adds scheduled_node column to instances table to pin each instance's Pod
-- and hostPath PV to a specific Kubernetes node. Required for multi-Worker
-- single-cluster expansion: once an instance is created on a node, all later
-- Pod recreations (Restart/Start/Update) must land on the same node so that
-- the hostPath PV at /tmp/clawreef/user-N/instance-M stays reachable.
--
-- Empty string means "no pin" (legacy single-node behaviour); the scheduler
-- code falls back to whatever node K8s picks. New instances always get a
-- non-empty value.

SET @instance_scheduled_node_column_exists = (
  SELECT COUNT(*)
  FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'instances'
    AND COLUMN_NAME = 'scheduled_node'
);
SET @instance_scheduled_node_column_sql = IF(
  @instance_scheduled_node_column_exists = 0,
  'ALTER TABLE instances ADD COLUMN scheduled_node VARCHAR(255) NOT NULL DEFAULT ''''  AFTER pod_ip',
  'SELECT 1'
);
PREPARE instance_scheduled_node_column_stmt FROM @instance_scheduled_node_column_sql;
EXECUTE instance_scheduled_node_column_stmt;
DEALLOCATE PREPARE instance_scheduled_node_column_stmt;

SET @instance_scheduled_node_index_exists = (
  SELECT COUNT(*)
  FROM information_schema.STATISTICS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'instances'
    AND INDEX_NAME = 'idx_instances_scheduled_node'
);
SET @instance_scheduled_node_index_sql = IF(
  @instance_scheduled_node_index_exists = 0,
  'ALTER TABLE instances ADD INDEX idx_instances_scheduled_node (scheduled_node)',
  'SELECT 1'
);
PREPARE instance_scheduled_node_index_stmt FROM @instance_scheduled_node_index_sql;
EXECUTE instance_scheduled_node_index_stmt;
DEALLOCATE PREPARE instance_scheduled_node_index_stmt;
