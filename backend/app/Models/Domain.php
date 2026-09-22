<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * 接入域名（多域名管理）。
 * ssl_status: none | pending | ok | expiring | failed
 */
class Domain extends Model
{
    protected $fillable = [
        'domain',
        'is_primary',
        'enabled',
        'ssl_status',
        'cert_expires_at',
        'last_error',
    ];

    protected function casts(): array
    {
        return [
            'is_primary' => 'boolean',
            'enabled' => 'boolean',
            'cert_expires_at' => 'datetime',
        ];
    }
}
