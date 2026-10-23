<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * 手工 socks 货源（共享型）。
 *
 * 一条记录 = 一个「共享套餐」的载体：挂到某个 SKU 上，那个 SKU 就能卖给多人共用
 * 同一套凭据。流量不由平台统计（交给 socks 机器/上游），所以这里没有用量字段。
 *
 * password 用 encrypted cast + $hidden：列表接口不回任何形式的密码。
 */
class ProxySocks extends Model
{
    protected $table = 'proxy_socks';

    protected $fillable = [
        'name',
        'host',
        'port',
        'username',
        'password',
        'country',
        'city',
        'note',
        'is_active',
    ];

    protected $hidden = ['password'];

    protected function casts(): array
    {
        return [
            'port' => 'integer',
            'password' => 'encrypted',
            'is_active' => 'boolean',
        ];
    }
}
