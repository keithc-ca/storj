// Copyright (C) 2023 Storj Labs, Inc.
// See LICENSE for copying information.

<template>
  <v-dialog v-model="dialog" activator="parent" width="auto" transition="fade-transition">
    <v-card rounded="xlg">

      <v-sheet>
        <v-card-item class="pl-7 py-4">
          <template v-slot:prepend>
            <v-card-title class="font-weight-bold">
              Project Limits
            </v-card-title>
          </template>

          <template v-slot:append>
            <v-btn icon="$close" variant="text" size="small" color="default" @click="dialog = false"></v-btn>
          </template>
        </v-card-item>
      </v-sheet>

      <v-divider></v-divider>

      <v-form v-model="valid" class="pa-7">
        <v-row>
          <v-col cols="12">
            <p>Enter limits for this project.</p>
          </v-col>
        </v-row>

        <v-row>
          <v-col cols="12" sm="6">
            <v-text-field label="Buckets" model-value="100" suffix="Buckets" variant="outlined"
              hide-details="auto"></v-text-field>
          </v-col>
          <v-col cols="12" sm="6">
            <v-text-field label="Storage" model-value="100" suffix="TB" variant="outlined"
              hide-details="auto"></v-text-field>
          </v-col>
          <v-col cols="12" sm="6">
            <v-text-field label="Download per month" model-value="300" suffix="TB" variant="outlined"
              hide-details="auto"></v-text-field>
          </v-col>
          <v-col cols="12" sm="6">
            <v-text-field label="Segments" model-value="100,000,000" variant="outlined"
              hide-details="auto"></v-text-field>
          </v-col>
          <v-col cols="12" sm="6">
            <v-text-field label="Rate" model-value="10,000" variant="outlined" hide-details="auto"></v-text-field>
          </v-col>
          <v-col cols="12" sm="6">
            <v-text-field label="Burst" model-value="50,000" variant="outlined" hide-details="auto"></v-text-field>
          </v-col>
        </v-row>

        <v-row>
          <v-col cols="12">
            <v-text-field model-value="56F82SR21Q284" label="Project ID" variant="solo-filled" flat readonly
              hide-details="auto"></v-text-field>
          </v-col>
        </v-row>
      </v-form>

      <v-divider></v-divider>

      <v-card-actions class="pa-7">
        <v-row>
          <v-col>
            <v-btn variant="outlined" color="default" block @click="dialog = false">Cancel</v-btn>
          </v-col>
          <v-col>
            <v-btn color="primary" variant="flat" block :loading="loading" @click="onButtonClick">Save</v-btn>
          </v-col>
        </v-row>
      </v-card-actions>
    </v-card>
  </v-dialog>

  <v-snackbar :timeout="7000" v-model="snackbar" color="error">
    {{ text }}
    <template v-slot:actions>
      <v-btn color="default" variant="text" @click="snackbar = false">
        Close
      </v-btn>
    </template>
  </v-snackbar>
</template>
  
<script>
export default {
  data() {
    return {
      snackbar: false,
      text: `Error. Cannot change project limits.`,
      dialog: false,
    }
  },
  methods: {
    onButtonClick() {
      this.snackbar = true;
      this.dialog = false;
    }
  }
}
</script>