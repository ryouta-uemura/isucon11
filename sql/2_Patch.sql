
ALTER TABLE `isu_condition` ADD `level` VARCHAR(8) NOT NULL DEFAULT "warning";
UPDATE `isu_condition` SET `level`="info" WHERE `condition`="is_dirty=false,is_overweight=false,is_broken=false";
UPDATE `isu_condition` SET `level`="critical" WHERE `condition`="is_dirty=true,is_overweight=true,is_broken=true";

-- optimize above
ALTER TABLE `isu_condition` ADD `condition_bits` TINYINT UNSIGNED  NOT NULL DEFAULT 0 AFTER `condition`; -- afterって何? (指定したカラムの次に配置される, localitiyのために重要なのか？？
ALTER TABLE `isu_condition` ADD `level_int` TINYINT UNSIGNED  NOT NULL DEFAULT 0 AFTER `level`; -- afterって何? (指定したカラムの次に配置される, localitiyのために重要なのか？？)
UPDATE `isu_condition` SET `condition_bits` = 
	IF(`condition` LIKE 'is_dirty=true,%', 1, 0 ) | 
	IF(`condition` LIKE '%,is_overweight=true,%', 2, 0 ) | 
	IF(`condition` LIKE '%,is_broken=true', 4, 0 ),
level_int = 
 CASE `level`
  WHEN 'info' THEN 0
  WHEN 'warning' THEN 1
  WHEN 'critical' THEN 2
  ELSE 0
 END;




