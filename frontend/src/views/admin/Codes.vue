<script setup>
import { ref } from 'vue'
import { useRoute } from 'vue-router'
import DiscountCodes from './DiscountCodes.vue'
import Giftcards from './Giftcards.vue'

// 「码管理」统一入口：折扣码（打折）与余额码（充余额）一个页面两个 tab。
// 支持 ?tab=balance 直接落到余额码（旧 /admin/giftcards 重定向带这个 query）。
const route = useRoute()
const tab = ref(route.query.tab === 'balance' ? 'balance' : 'discount')
</script>

<template>
  <div class="cd">
    <div class="cd-tabs" role="tablist">
      <button
        class="cd-tab"
        :class="{ 'is-active': tab === 'discount' }"
        role="tab"
        :aria-selected="tab === 'discount'"
        @click="tab = 'discount'"
      >
        折扣码
      </button>
      <button
        class="cd-tab"
        :class="{ 'is-active': tab === 'balance' }"
        role="tab"
        :aria-selected="tab === 'balance'"
        @click="tab = 'balance'"
      >
        余额码
      </button>
    </div>
    <DiscountCodes v-if="tab === 'discount'" />
    <Giftcards v-else />
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/Codes.css"></style>
