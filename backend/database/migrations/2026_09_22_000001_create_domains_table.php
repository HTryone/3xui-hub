<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（例如从备份导入，或表由其它途径建过）→ 跳过创建，避免 1050 撞车。
        // 这里用 if 包住建表而不是提前 return：下面的 seed 在表已存在时仍要照跑。
        if (! Schema::hasTable('domains')) {
            Schema::create('domains', function (Blueprint $table) {
                $table->integer('id')->unsigned()->primary()->autoIncrement();
                $table->string('domain', 190)->unique();
                $table->boolean('is_primary')->default(false);
                $table->boolean('enabled')->default(true);
                $table->string('ssl_status', 16)->default('none'); // none|pending|ok|expiring|failed
                $table->dateTime('cert_expires_at')->nullable();
                $table->text('last_error')->nullable();
                $table->timestamps();
            });
        }

        // 幂等 seed：表为空且 config('app.url') 的 host 合法（非 localhost / 非 IP）时插入当前主域
        if (DB::table('domains')->count() === 0) {
            $url = (string) config('app.url');
            $host = parse_url($url, PHP_URL_HOST);

            $isIphost = $host !== null && filter_var($host, FILTER_VALIDATE_IP) !== false;
            if ($host && $host !== 'localhost' && !$isIphost) {
                $scheme = strtolower((string) parse_url($url, PHP_URL_SCHEME));
                DB::table('domains')->insert([
                    'domain' => strtolower($host),
                    'is_primary' => 1,
                    'enabled' => 1,
                    'ssl_status' => $scheme === 'https' ? 'ok' : 'none',
                    'created_at' => now(),
                    'updated_at' => now(),
                ]);
            }
        }
    }

    public function down(): void
    {
        Schema::dropIfExists('domains');
    }
};
