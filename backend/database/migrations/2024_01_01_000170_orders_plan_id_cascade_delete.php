<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Support\Facades\DB;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：这条是裸 SQL，没有 Schema 的语法糖可挡，直接问 information_schema。
        // 外键已经存在且已经是 CASCADE = 这条迁移的效果已经达成 → 整段跳过。
        // 不挡的话，重跑时最后那句 ADD CONSTRAINT 会撞 1826「Duplicate foreign key constraint name」，
        // 前面的两句又被 try/catch 吞掉，migrate 就停在这条上，后面的迁移全不跑。
        if ($this->ordersPlanIdDeleteRule() === 'CASCADE') {
            return;
        }

        // 先尝试删外键约束（服务器有），再删索引（本地只有索引）
        try { DB::statement('ALTER TABLE orders DROP FOREIGN KEY orders_plan_id_foreign'); } catch (\Exception $e) {}
        try { DB::statement('ALTER TABLE orders DROP INDEX orders_plan_id_foreign'); } catch (\Exception $e) {}
        DB::statement('ALTER TABLE orders ADD CONSTRAINT orders_plan_id_foreign FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE');
    }

    /**
     * orders_plan_id_foreign 当前的 ON DELETE 规则；约束不存在时返回 null（说明还没建过，该往下跑）。
     */
    private function ordersPlanIdDeleteRule(): ?string
    {
        $row = DB::selectOne(
            'SELECT DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS
             WHERE CONSTRAINT_SCHEMA = DATABASE()
               AND TABLE_NAME = ?
               AND CONSTRAINT_NAME = ?',
            ['orders', 'orders_plan_id_foreign']
        );

        return $row->DELETE_RULE ?? null;
    }

    public function down(): void
    {
        DB::statement('ALTER TABLE orders DROP FOREIGN KEY orders_plan_id_foreign');
        DB::statement('ALTER TABLE orders ADD INDEX orders_plan_id_foreign (plan_id)');
    }
};
