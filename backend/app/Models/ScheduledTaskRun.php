<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

class ScheduledTaskRun extends Model
{
    public $timestamps = false;

    protected $fillable = [
        'command', 'status', 'error', 'duration_ms', 'ran_at',
    ];
}
