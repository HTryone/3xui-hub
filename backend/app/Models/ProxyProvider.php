<?php

namespace App\Models;

use App\Services\Webshare\WebshareClient;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;

/**
 * 代理上游账号（站长自己的 Webshare 账号，可配多个）。
 *
 * api_token 用 encrypted cast 加密存储；对外展示只给「是否已配置」，
 * 不返回任何形式的明文或密文（见 Admin\UpstreamWebshareController::presentProvider）。
 */
class ProxyProvider extends Model
{
    protected $table = 'proxy_providers';

    protected $fillable = [
        'name',
        'driver',
        'api_token',
        'enabled',
        'last_status',
        'last_error',
        'last_checked_at',
    ];

    protected $hidden = ['api_token'];

    protected function casts(): array
    {
        return [
            'api_token' => 'encrypted',
            'enabled' => 'boolean',
            'last_checked_at' => 'datetime',
        ];
    }

    public function accounts(): HasMany
    {
        return $this->hasMany(ProxyAccount::class, 'provider_id');
    }

    /** 按本账号的 token 造一个上游客户端（测试里也走这条，不另开分支）。 */
    public function client(): WebshareClient
    {
        return new WebshareClient(
            (string) $this->api_token,
            $this->baseUrl(),
            // 出口代理 / 自定义 CA：国内机房访问 proxy.webshare.io 需要，见 config/panel.php
            (string) config('panel.webshare.proxy', '') ?: null,
            (string) config('panel.webshare.ca_bundle', '') ?: null,
        );
    }

    /** 上游 base URL（官方 v2 地址；自建反代可构造 WebshareClient 时传参覆盖）。 */
    public function baseUrl(): string
    {
        return WebshareClient::BASE_URL;
    }
}
