INSERT INTO `latest_isu_condition`
  (`jia_isu_uuid`, `timestamp`, `is_sitting`,`condition_bits`, `level_int`, `message`)
SELECT  c.`jia_isu_uuid`, c.`timestamp`, c.`is_sitting`, c.`condition_bits`, c.`level_int`, c.`message`
FROM isu_condition c
JOIN (
	SELECT jia_isu_uuid , MAX(timestamp) as max_timestamp
	FROM isu_condition
	GROUP BY jia_isu_uuid
) latest
ON latest.jia_isu_uuid = c.jia_isu_uuid
 AND latest.max_timestamp = c.`timestamp`;



