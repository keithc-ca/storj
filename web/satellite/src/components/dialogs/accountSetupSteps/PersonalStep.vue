// Copyright (C) 2023 Storj Labs, Inc.
// See LICENSE for copying information.

<template>
    <v-container>
        <v-form v-model="formValid" @submit.prevent="emit('next')">
            <v-row justify="center">
                <v-col class="text-center py-4">
                    <UserRound height="50" width="50" class="rounded-xlg bg-background pa-3 border" />
                    <p class="text-overline mt-2 mb-1">
                        Personal Account
                    </p>
                    <h2 class="pb-3">Great, almost there.</h2>
                    <p>Please complete your account information.</p>
                </v-col>
            </v-row>

            <v-row justify="center">
                <v-col cols="12" sm="8" md="6" lg="4">
                    <v-text-field
                        id="Name"
                        v-model="name"
                        :rules="[RequiredRule, MaxNameLengthRule]"
                        label="Name"
                        placeholder="Enter your name"
                        required
                    />
                    <v-select
                        v-model="useCase"
                        :items="[ 'Active archive', 'Backup & recovery', 'CDN origin', 'Generative AI', 'Media workflows', 'Other']"
                        label="Use Case (optional)"
                        placeholder="Select your use case"
                        variant="outlined"
                        class="my-1"
                        hide-details="auto"
                        @update:model-value="(v) => analyticsStore.eventTriggered(AnalyticsEvent.USE_CASE_SELECTED, { useCase: v })"
                    />
                    <v-text-field
                        v-if="useCase === 'Other'"
                        v-model="otherUseCase"
                        label="Specify Other Use Case"
                        variant="outlined"
                        autofocus
                        class="my-1"
                        hide-details="auto"
                    />
                </v-col>
            </v-row>

            <v-row justify="center">
                <v-col cols="6" sm="4" md="3" lg="2">
                    <v-btn
                        size="large"
                        variant="outlined"
                        :prepend-icon="ChevronLeft"
                        color="default"
                        :disabled="loading"
                        block
                        @click="emit('back')"
                    >
                        Back
                    </v-btn>
                </v-col>
                <v-col cols="6" sm="4" md="3" lg="2">
                    <v-btn
                        size="large"
                        :append-icon="ChevronRight"
                        :loading="loading"
                        :disabled="!formValid"
                        block
                        type="submit"
                    >
                        Continue
                    </v-btn>
                </v-col>
            </v-row>
        </v-form>
    </v-container>
</template>

<script setup lang="ts">
import { VBtn, VCol, VContainer, VForm, VRow, VSelect, VTextField } from 'vuetify/components';
import { ref } from 'vue';
import { ChevronLeft, ChevronRight, UserRound } from 'lucide-vue-next';

import { MaxNameLengthRule, RequiredRule } from '@/types/common';
import { AuthHttpApi } from '@/api/auth';
import { AnalyticsEvent } from '@/utils/constants/analyticsEventNames';
import { useAnalyticsStore } from '@/store/modules/analyticsStore';
import { useConfigStore } from '@/store/modules/configStore';

const auth = new AuthHttpApi();

const analyticsStore = useAnalyticsStore();
const configStore = useConfigStore();

withDefaults(defineProps<{
    loading?: boolean,
}>(), {
    loading: false,
});

const name = defineModel<string>('name', { required: true });
const useCase = defineModel<string | undefined>('useCase', { required: true });
const otherUseCase = defineModel<string | undefined>('otherUseCase', { required: true });

const emit = defineEmits<{
    (event: 'next'): void,
    (event: 'back'): void,
}>();

const formValid = ref(false);

async function setupAccount() {
    await auth.setupAccount({
        fullName: name.value,
        storageUseCase: useCase.value,
        otherUseCase: otherUseCase.value,
        haveSalesContact: false,
        interestedInPartnering: false,
        isProfessional: false,
    }, configStore.state.config.csrfToken);

    analyticsStore.eventTriggered(AnalyticsEvent.PERSONAL_INFO_SUBMITTED);
}

function validate() {
    return formValid.value;
}

defineExpose({
    validate,
    setup: setupAccount,
});
</script>
