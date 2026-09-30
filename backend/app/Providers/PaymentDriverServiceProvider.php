<?php

namespace App\Providers;

use App\Drivers\DriverRegistry;
use App\Drivers\Payments\EpayV1Driver;
use App\Drivers\Payments\PayindexDriver;
use Illuminate\Support\ServiceProvider;

/**
 * 支付驱动注册：新增支付协议在此 register 进 DriverRegistry。
 */
class PaymentDriverServiceProvider extends ServiceProvider
{
    public function boot(): void
    {
        $registry = $this->app->make(DriverRegistry::class);

        $registry->register(new PayindexDriver());
        $registry->register(new EpayV1Driver());
    }
}
