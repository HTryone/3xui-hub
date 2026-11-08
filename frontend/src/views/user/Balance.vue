<script setup>
/**
 * Balance — 用户端「账户余额」二级页。
 * 「我的」页只放入口，余额 / 充值 / 兑换码 / 流水整块收进这里（用户点进才看到余额数字）。
 */
import BalancePanel from '@/components/user/BalancePanel.vue'
</script>

<template>
  <div class="bl uu-slide-up">
    <BalancePanel />
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Balance.css"></style>
