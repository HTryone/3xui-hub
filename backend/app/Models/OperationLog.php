<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

class OperationLog extends Model
{
    public $timestamps = false;

    protected $fillable = [
        'type', 'actor_id', 'actor_name', 'action',
        'method', 'path', 'status', 'error', 'ip', 'created_at',
    ];

    protected $casts = [
        'status' => 'integer',
        'created_at' => 'datetime',
    ];
}
