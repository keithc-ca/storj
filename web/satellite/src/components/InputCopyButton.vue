// Copyright (C) 2023 Storj Labs, Inc.
// See LICENSE for copying information.

<template>
    <v-tooltip v-model="isTooltip" location="start">
        <template #activator="{ props: activatorProps }">
            <v-btn
                v-bind="activatorProps"
                :icon="justCopied ? Check : Copy"
                variant="text"
                density="compact"
                aria-roledescription="copy-btn"
                :color="justCopied ? 'success' : 'primary'"
                @click="onCopy"
            />
        </template>
        {{ justCopied ? 'Copied!' : 'Copy' }}
    </v-tooltip>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue';
import { VTooltip, VBtn } from 'vuetify/components';
import { Check, Copy } from 'lucide-vue-next';

import { useAnalyticsStore } from '@/store/modules/analyticsStore';
import { AnalyticsEvent } from '@/utils/constants/analyticsEventNames';

const props = defineProps<{
    value: string;
    tooltipDisabled?: boolean;
}>();

const copiedTimeout = ref<NodeJS.Timeout>();
const justCopied = computed<boolean>(() => copiedTimeout.value !== undefined);

const isTooltip = (() => {
    const internal = ref<boolean>(false);
    return computed<boolean>({
        get: () => (internal.value || justCopied.value) && !props.tooltipDisabled,
        set: v => internal.value = v,
    });
})();

const analyticsStore = useAnalyticsStore();

/**
 * Saves value to clipboard.
 */
function onCopy(): void {
    navigator.clipboard.writeText(props.value);
    analyticsStore.eventTriggered(AnalyticsEvent.COPY_TO_CLIPBOARD_CLICKED);

    if (copiedTimeout.value) clearTimeout(copiedTimeout.value);
    copiedTimeout.value = setTimeout(() => {
        copiedTimeout.value = undefined;
    }, 750);
}
</script>
