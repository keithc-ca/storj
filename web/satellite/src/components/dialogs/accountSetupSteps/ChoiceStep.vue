// Copyright (C) 2023 Storj Labs, Inc.
// See LICENSE for copying information.

<template>
    <v-container>
        <v-row justify="center">
            <v-col class="text-center py-4">
                <icon-storj-logo height="50" width="50" class="rounded-xlg bg-background pa-2 border" />
                <p class="text-overline mt-2 mb-1">
                    Welcome to Storj
                </p>
                <h2 class="pb-3">Start by setting up your account</h2>
                <p>Select your account type to customize your Storj experience.</p>
            </v-col>
        </v-row>

        <v-row justify="center">
            <v-col cols="12" sm="6" lg="4">
                <v-card id="personal" class="px-3 py-5" @click="typeSelected(OnboardingStep.PersonalAccountForm)">
                    <v-card-item>
                        <div>
                            <UserRound height="50" width="50" class="rounded-xlg bg-background pa-3 border" />

                            <p class="text-overline mt-2 mb-1">
                                Personal
                            </p>
                            <p class="text-h6 mb-2">
                                I'm using Storj for myself.
                            </p>
                            <p class="text-body-2">Securely store, backup, share, stream, and collaborate on files and media from any device.</p>
                        </div>
                    </v-card-item>
                    <v-card-item>
                        <v-btn :append-icon="ChevronRight">Continue</v-btn>
                    </v-card-item>
                </v-card>
            </v-col>

            <v-col cols="12" sm="6" lg="4">
                <v-card id="business" class="px-3 py-5" @click="typeSelected(OnboardingStep.BusinessAccountForm)">
                    <v-card-item>
                        <div>
                            <Building2 height="50" width="50" class="rounded-xlg bg-background pa-3 border" />

                            <p class="text-overline mt-2 mb-1">
                                Business
                            </p>
                            <p class="text-h6 mb-2">
                                I'm using Storj for business.
                            </p>
                            <p class="text-body-2 ">Save your company 80% or more on cloud storage costs with better security and performance.</p>
                        </div>
                    </v-card-item>
                    <v-card-item>
                        <v-btn :append-icon="ChevronRight">Continue</v-btn>
                    </v-card-item>
                </v-card>
            </v-col>
        </v-row>
    </v-container>
</template>

<script setup lang="ts">
import { VBtn, VCard, VCardItem, VCol, VContainer, VRow } from 'vuetify/components';
import { Building2, ChevronRight, UserRound } from 'lucide-vue-next';

import { OnboardingStep } from '@/types/users';
import { AnalyticsEvent } from '@/utils/constants/analyticsEventNames';
import { useAnalyticsStore } from '@/store/modules/analyticsStore';

import IconStorjLogo from '@/components/icons/IconStorjLogo.vue';

const analyticsStore = useAnalyticsStore();

const emit = defineEmits<{
    select: [OnboardingStep.BusinessAccountForm | OnboardingStep.PersonalAccountForm];
}>();

function typeSelected(type: OnboardingStep.BusinessAccountForm | OnboardingStep.PersonalAccountForm) {
    emit('select', type);

    let event: AnalyticsEvent;
    switch (type) {
    case OnboardingStep.BusinessAccountForm:
        event = AnalyticsEvent.BUSINESS_SELECTED;
        break;
    case OnboardingStep.PersonalAccountForm:
        event = AnalyticsEvent.PERSONAL_SELECTED;
        break;
    default:
        return;
    }
    analyticsStore.eventTriggered(event);
}
</script>
